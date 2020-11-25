package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kataras/tablewriter"
	"github.com/sirupsen/logrus"

	prowconfig "k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/flagutil"
	prowplugins "k8s.io/test-infra/prow/plugins"

	"github.com/openshift/ci-tools/pkg/deprecatetemplates"
)

type options struct {
	prowJobConfigDir     string
	prowConfigPath       string
	prowPluginConfigPath string
	allowlistPath        string
	prune                bool
	printStats           bool
	hideTotals           bool
	blockNewJobs         flagutil.Strings

	help bool
}

func bindOptions(fs *flag.FlagSet) *options {
	opt := &options{}

	fs.StringVar(&opt.prowJobConfigDir, "prow-jobs-dir", "", "Path to a root of directory structure with Prow job config files (ci-operator/jobs in openshift/release)")
	fs.StringVar(&opt.prowConfigPath, "prow-config-path", "", "Path to the Prow configuration file")
	fs.StringVar(&opt.prowPluginConfigPath, "prow-plugin-config-path", "", "Path to the Prow plugin configuration file")
	fs.StringVar(&opt.allowlistPath, "allowlist-path", "", "Path to template deprecation allowlist")
	fs.Var(&opt.blockNewJobs, "block-new-jobs", "If set, new jobs will be added to this blocker instead of to the 'unknown blocker' list. Can be set multiple times and can have either JIRA or JIRA:description form")
	fs.BoolVar(&opt.prune, "prune", false, "If set, remove from allowlist all jobs that either no longer exist or no longer use a template")
	fs.BoolVar(&opt.printStats, "stats", false, "If true, print template usage stats")
	fs.BoolVar(&opt.hideTotals, "hide-totals", false, "If true, hide totals in template usage stats")

	return opt
}

func (o *options) validate() error {
	for param, value := range map[string]string{
		"--prow-jobs-dir":           o.prowJobConfigDir,
		"--prow-config-path":        o.prowConfigPath,
		"--prow-plugin-config-path": o.prowPluginConfigPath,
		"--allowlist-path":          o.allowlistPath,
	} {
		if value == "" {
			return fmt.Errorf("mandatory argument %s was not set", param)
		}
	}

	return nil
}

func main() {
	opt := bindOptions(flag.CommandLine)
	flag.Parse()

	if opt.help {
		flag.Usage()
		os.Exit(0)
	}

	if err := opt.validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid parameters")
	}

	agent := prowplugins.ConfigAgent{}
	if err := agent.Load(opt.prowPluginConfigPath, true); err != nil {
		logrus.WithError(err).Fatal("Failed to read Prow plugin configuration")
	}
	pluginCfg := agent.Config().ConfigUpdater

	prowCfg, err := prowconfig.Load(opt.prowConfigPath, opt.prowJobConfigDir)
	if err != nil {
		logrus.WithError(err).Fatal("failed to load Prow configuration")
	}

	newJobBlockers := deprecatetemplates.JiraHints{}
	for _, value := range opt.blockNewJobs.Strings() {
		jira := strings.SplitN(value, ":", 2)
		switch len(jira) {
		case 1:
			newJobBlockers[jira[0]] = ""
		case 2:
			newJobBlockers[jira[0]] = jira[1]
		default:
			logrus.WithError(err).Fatal("invalid --block-new-jobs value")
		}
	}

	enforcer, err := deprecatetemplates.NewEnforcer(opt.allowlistPath, newJobBlockers)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to initialize template deprecator")
	}

	enforcer.LoadTemplates(pluginCfg)
	enforcer.ProcessJobs(prowCfg)

	if opt.prune {
		enforcer.Prune()
	}

	if opt.printStats {
		header, footer, data := enforcer.Stats(opt.hideTotals)
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader(header)
		table.SetFooter(footer)
		table.AppendBulk(data)
		table.Render()
	}

	if err := enforcer.SaveAllowlist(opt.allowlistPath); err != nil {
		logrus.WithError(err).Fatal("Failed to save template deprecation allowlist")
	}
}
