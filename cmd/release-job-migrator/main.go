package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	prowconfig "k8s.io/test-infra/prow/config"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	configlib "github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/jobconfig"
)

type GeneratorConfig struct {
	// Configs defines the configuration needed for a specified test name
	Configs map[string]testConfig `json:"config"`
}

type testConfig struct {
	// Steps defines the multistage test step configuration to use for this job
	Steps *api.MultiStageTestConfiguration `json:"steps"`
	// BaseImages defines any images that this test relies on. Omit the "Name" and/or
	// "Namespace" fields to have them autofilled based on the job name
	BaseImages map[string]api.ImageStreamTagReference `json:"base_images,omitempty"`
}

type testsAndBaseImages struct {
	tests      []api.TestStepConfiguration
	baseImages map[string]api.ImageStreamTagReference
}

type jobInfo struct {
	As      string
	Product string
	Version string
}

type options struct {
	config        string
	ciopConfigDir string
	rcConfigDir   string
	jobDir        string
}

func gatherOptions() (options, error) {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.config, "config", "", "Path to config file")
	fs.StringVar(&o.ciopConfigDir, "ci-op-configs", "", "Path to ci-operator config files")
	fs.StringVar(&o.rcConfigDir, "rc-configs", "", "Path to release-controller release config files")
	fs.StringVar(&o.jobDir, "jobs", "", "Path to ci-operator jobs")
	return o, fs.Parse(os.Args[1:])
}

func validateOptions(o options) error {
	if len(o.config) == 0 {
		return errors.New("--config must be set")
	}
	if len(o.ciopConfigDir) == 0 {
		return errors.New("--ci-op-configs must be set")
	}
	if len(o.rcConfigDir) == 0 {
		return errors.New("--rc-configs must be set")
	}
	if len(o.jobDir) == 0 {
		return errors.New("--jobs must be set")
	}
	return nil
}

var versionRegex = regexp.MustCompile(`4.\d`)

func getJobInfo(jobName string) (jobInfo, bool) {
	// release jobs follow the format of "release-openshift-product-installer-testname-version"
	if !strings.HasPrefix(jobName, "release-openshift-") {
		return jobInfo{}, false
	}
	splitName := strings.Split(jobName, "-")
	if len(splitName) < 6 {
		return jobInfo{}, false
	}
	version := splitName[len(splitName)-1]
	if !versionRegex.MatchString(version) {
		return jobInfo{}, false
	}
	return jobInfo{
		As:      strings.Join(splitName[4:len(splitName)-1], "-"),
		Product: splitName[2],
		Version: splitName[len(splitName)-1],
	}, true
}

func metadataFromJobInfo(info jobInfo) *api.Metadata {
	return &api.Metadata{
		Org:     "openshift",
		Repo:    "release",
		Branch:  "master",
		Variant: fmt.Sprintf("%s-%s", info.Product, info.Version),
	}
}

func newDataWithInfoFromFilename(filename string) configlib.DataWithInfo {
	// identify product and version from variant
	variant := strings.Split(strings.TrimSuffix(filename, ".yaml"), "__")[1]
	splitVariant := strings.Split(variant, "-")
	identifier := splitVariant[0]
	var product api.ReleaseProduct
	var stream api.ReleaseStream
	var namespace string
	switch identifier {
	case "ocp":
		product = api.ReleaseProductOCP
		stream = api.ReleaseStreamNightly
		namespace = "ocp"
	case "origin":
		product = api.ReleaseProductOCP
		stream = api.ReleaseStreamCI
		namespace = "ocp"
	case "okd":
		product = api.ReleaseProductOKD
		stream = api.ReleaseStreamOKD
		namespace = "origin"
	}
	version := splitVariant[1]
	return configlib.DataWithInfo{
		Info: config.Info{
			Metadata: api.Metadata{
				Org:     "openshift",
				Repo:    "release",
				Branch:  "master",
				Variant: variant,
			},
		},
		Configuration: api.ReleaseBuildConfiguration{
			InputConfiguration: api.InputConfiguration{
				BaseImages: map[string]api.ImageStreamTagReference{
					"base": {
						Name:      version,
						Namespace: namespace,
						Tag:       "base",
					},
				},
				Releases: map[string]api.UnresolvedRelease{
					"latest": {
						Candidate: &api.Candidate{
							Product: product,
							Stream:  stream,
							Version: version,
						},
					},
				},
			},
			Resources: api.ResourceConfiguration{
				"*": api.ResourceRequirements{
					Requests: api.ResourceList{
						"cpu":    "100m",
						"memory": "200Mi",
					},
				},
			},
			Metadata: api.Metadata{
				Org:     "openshift",
				Repo:    "release",
				Branch:  "master",
				Variant: variant,
			},
		},
	}
}

func updateBaseImages(newImages, ciOpImages, replacementImages map[string]api.ImageStreamTagReference, version string) error {
	for name, newImage := range newImages {
		if newImage.Namespace == "" {
			newImage.Namespace = "ocp"
		}
		if newImage.Name == "" {
			newImage.Name = version
		}
		// check if image already exists
		baseImage, ok := replacementImages[name]
		if !ok {
			baseImage, ok = ciOpImages[name]
		}
		if ok {
			if baseImage.As != newImage.As || baseImage.Name != newImage.Name || baseImage.Namespace != newImage.Namespace || baseImage.Tag != newImage.Tag {
				return fmt.Errorf("2 different images detected for base image %s: (%+v) and (%+v)", name, baseImage, newImage)
			}
		} else {
			replacementImages[name] = newImage
		}
	}
	return nil
}

func run(o options) error {
	// key: filename
	jobs := make(map[string]prowconfig.JobConfig)
	if err := filepath.Walk(o.jobDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() != filepath.Base(o.jobDir) {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, "-periodics.yaml") {
			return nil
		}
		raw, err := ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", info.Name(), err)
		}
		periodics := prowconfig.JobConfig{}
		if err := yaml.UnmarshalStrict(raw, &periodics); err != nil {
			return fmt.Errorf("failed to unmarshal file %s: %w", info.Name(), err)
		}
		jobs[info.Name()] = periodics
		return nil
	}); err != nil {
		return fmt.Errorf("failed to load periodic job configs: %w", err)
	}

	rawConfig, err := ioutil.ReadFile(o.config)
	if err != nil {
		return fmt.Errorf("failed to read config file %s: %w", o.config, err)
	}
	generatorConfig := GeneratorConfig{}
	if err := yaml.UnmarshalStrict(rawConfig, &generatorConfig); err != nil {
		return fmt.Errorf("failed to unmarshal config file %s: %w", o.config, err)
	}
	ciopConfigs, err := configlib.LoadDataByFilename(o.ciopConfigDir)
	if err != nil {
		return fmt.Errorf("failed to load ci-operator configs: %w", err)
	}

	// store replacement info for each ci-op config
	replacements := make(map[string]testsAndBaseImages)
	// key: old jobname, value: new (generated jobname)
	replacedJobs := make(map[string]string)
	// list of test names for detected release jobs with no configuration
	configlessTests := sets.NewString()
	for _, jobConfig := range jobs {
		for _, periodic := range jobConfig.Periodics {
			info, isReleaseJob := getJobInfo(periodic.Name)
			if !isReleaseJob {
				continue
			}
			// avoid jobs that do more than just run ci-operator
			if periodic.Spec.Containers[0].Command[0] != "ci-operator" {
				logrus.Warnf("periodic job %s has a command != \"ci-operator\", ignoring", periodic.Name)
				continue
			}
			// check if an UNRESOLVED_CONFIG exists and populate an env var map to handle configs with env vars
			envs := make(map[string]string)
			isUnresolved := false
			for _, env := range periodic.Spec.Containers[0].Env {
				envs[env.Name] = env.Value
				if env.Name == "UNRESOLVED_CONFIG" {
					isUnresolved = true
				}
			}
			filename := metadataFromJobInfo(info).Basename()
			if _, ok := replacements[filename]; !ok {
				replacements[filename] = testsAndBaseImages{
					baseImages: make(map[string]api.ImageStreamTagReference),
				}
			}
			var conf testConfig
			if isUnresolved {
				target := ""
				for _, arg := range periodic.Spec.Containers[0].Args {
					if strings.Split(arg, "=")[0] == "--target" {
						target = strings.Split(arg, "=")[1]
						// args can contain env vars; make sure they're replaced
						for name, value := range envs {
							target = strings.ReplaceAll(target, fmt.Sprintf("$(%s)", name), value)
						}
					}
				}
				if target == "" {
					return fmt.Errorf("found UNRESOLVED_CONFIG for job %s but could not identify target job", periodic.Name)
				}
				// replace all env vars in UNRESOLVED CONFIG
				unresolvedConfig := envs["UNRESOLVED_CONFIG"]
				for name, value := range envs {
					unresolvedConfig = strings.ReplaceAll(unresolvedConfig, fmt.Sprintf("$(%s)", name), value)
				}
				unmarshaledConfig := api.ReleaseBuildConfiguration{}
				if err := yaml.Unmarshal([]byte(unresolvedConfig), &unmarshaledConfig); err != nil {
					return fmt.Errorf("failed to unmarshal UNRESOLVED_CONFIG for periodic %s: %w", periodic.Name, err)
				}
				for _, test := range unmarshaledConfig.Tests {
					if test.As == target {
						conf.Steps = test.MultiStageTestConfiguration
					}
				}
				if conf.Steps == nil {
					return fmt.Errorf("failed to identify multi-stage test configuration for job %s", periodic.Name)
				}
				if err := updateBaseImages(conf.BaseImages, ciopConfigs[filename].Configuration.BaseImages, replacements[filename].baseImages, info.Version); err != nil {
					return err
				}
			} else {
				var ok bool
				conf, ok = generatorConfig.Configs[info.As]
				if !ok {
					configlessTests.Insert(info.As)
					continue
				}
				if err := updateBaseImages(conf.BaseImages, ciopConfigs[filename].Configuration.BaseImages, replacements[filename].baseImages, info.Version); err != nil {
					return err
				}
			}
			cron := periodic.Cron
			if cron == "" {
				cron = fmt.Sprintf("@every %s", periodic.Interval)
			}
			// check that test does not already exist in config
			combinedTests := append(replacements[filename].tests, ciopConfigs[filename].Configuration.Tests...)
			for _, step := range combinedTests {
				if step.As == info.As {
					return fmt.Errorf("error adding periodic %s: test name %s already exists", periodic.Name, step.As)
				}
			}
			testsAndImages := replacements[filename]
			testsAndImages.tests = append(testsAndImages.tests, api.TestStepConfiguration{
				As:                          info.As,
				MultiStageTestConfiguration: conf.Steps,
				Cron:                        &cron,
			})
			replacements[filename] = testsAndImages
			replacedJobs[periodic.Name] = metadataFromJobInfo(info).JobName(jobconfig.PeriodicPrefix, info.As)
		}
	}

	for filename, replacement := range replacements {
		_, ok := ciopConfigs[filename]
		if !ok {
			ciopConfigs[filename] = newDataWithInfoFromFilename(filename)
		}
		updatedConfig := ciopConfigs[filename].Configuration
		updatedConfig.Tests = append(updatedConfig.Tests, replacement.tests...)
		for name, ist := range replacement.baseImages {
			if _, ok := updatedConfig.BaseImages[name]; !ok {
				updatedConfig.BaseImages[name] = ist
			}
		}
		raw, err := yaml.Marshal(updatedConfig)
		if err != nil {
			return fmt.Errorf("failed to marshal updated config for file %s: %w", filename, err)
		}
		if err := ioutil.WriteFile(filepath.Join(o.ciopConfigDir, filename), raw, 0644); err != nil {
			return fmt.Errorf("failed to write updated config file %s: %w", filename, err)
		}
	}

	// delete old jobs
	for filename, oldConfig := range jobs {
		newConfig := prowconfig.JobConfig{}
		// remake periodic jobconfig excluding replaced jobs
		for _, job := range oldConfig.Periodics {
			if _, ok := replacedJobs[job.Name]; !ok {
				newConfig.Periodics = append(newConfig.Periodics, job)
			}
		}
		raw, err := yaml.Marshal(newConfig)
		if err != nil {
			return fmt.Errorf("failed to marshal updated jobconfig for file %s: %w", filename, err)
		}
		if err := ioutil.WriteFile(filepath.Join(o.jobDir, filename), raw, 0644); err != nil {
			return fmt.Errorf("failed to write updated jobconfig file %s: %w", filename, err)
		}
	}

	// update release-controller configs
	if err := filepath.Walk(o.rcConfigDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() != filepath.Base(o.rcConfigDir) {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", info.Name(), err)
		}
		for oldName, newName := range replacedJobs {
			raw = bytes.ReplaceAll(raw, []byte(oldName), []byte(newName))
		}
		if err := ioutil.WriteFile(path, raw, 0644); err != nil {
			return fmt.Errorf("failed to write updated release-controller config file %s: %w", filepath.Base(path), err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to process release-controller files: %w", err)
	}

	// print out replacement info
	if len(replacedJobs) == 0 && len(configlessTests) == 0 {
		fmt.Println("No non-generated release-controller jobs detected.")
		return nil
	}

	if len(replacedJobs) > 0 {
		fmt.Println("The following jobs have been replaced:")
		// print in alphabetical order
		var sortedNames []string
		for oldName := range replacedJobs {
			sortedNames = append(sortedNames, oldName)
		}
		sort.Strings(sortedNames)
		for _, oldName := range sortedNames {
			fmt.Printf("%s -> %s\n", oldName, replacedJobs[oldName])
		}
	} else {
		fmt.Println("No jobs detected with matching config.")
	}

	if len(configlessTests) > 0 {
		fmt.Printf("\nThe following tests do not have entries in the generator config:\n%v\n", configlessTests.List())
	}

	// keep this message at the end to make sure it is seen by whoever is running the command
	if len(replacedJobs) > 0 {
		fmt.Printf("\nPlease run `make update` to regenerate job configs using the updated ci-operator configs.")
	}
	fmt.Println()
	return nil
}

func main() {
	o, err := gatherOptions()
	if err != nil {
		logrus.WithError(err).Fatal("failed go gather options")
	}
	if err := validateOptions(o); err != nil {
		logrus.WithError(err).Fatal("invalid options")
	}
	if err := run(o); err != nil {
		logrus.Fatalf("failed to generate jobs: %v", err)
	}
}
