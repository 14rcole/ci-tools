package stepgraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/spyglass/api"
	"k8s.io/test-infra/prow/spyglass/lenses"
	"sigs.k8s.io/yaml"

	citoolsapi "github.com/openshift/ci-tools/pkg/api"
)

const (
	name     = "steps"
	title    = "CI-Operator steps"
	priority = 6
)

// Lens is the implementation of a JUnit-rendering Spyglass lens.
type Lens struct{}

// Config returns the lens's configuration.
func (lens Lens) Config() lenses.LensConfig {
	return lenses.LensConfig{
		Name:     name,
		Title:    title,
		Priority: priority,
	}
}

var tmpl *template.Template

func init() {
	data, err := staticTemplateHtmlBytes()
	if err != nil {
		panic(fmt.Sprintf("failed to gzunip template: %v", err))
	}
	tmpl = template.Must(template.New("template").Parse(string(data)))
}

// Header renders the content of <head> from template.html.
func (lens Lens) Header(artifacts []api.Artifact, _ string, config json.RawMessage) string {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "header", nil); err != nil {
		return fmt.Sprintf("<!-- FAILED EXECUTING HEADER TEMPLATE: %v -->", err)
	}
	return buf.String()
}

// Callback does nothing.
func (lens Lens) Callback(artifacts []api.Artifact, resourceDir string, data string, config json.RawMessage) string {
	return ""
}

// Body renders the <body>
func (lens Lens) Body(artifacts []api.Artifact, resourceDir string, data string, config json.RawMessage) string {
	if len(artifacts) != 1 {
		logrus.WithField("artifacts_count", len(artifacts)).Error("Expected exactly one artifact")
		return ""
	}

	serializedGraph, err := artifacts[0].ReadAll()
	if err != nil {
		logrus.WithError(err).Error("Failed to read artifact")
		return ""
	}

	graph := []Step{}
	if err := json.Unmarshal(serializedGraph, &graph); err != nil {
		logrus.WithError(err).Error("Failed to unmarshal graph")
		return ""
	}

	sort.Slice(graph, func(i, j int) bool {
		return graph[i].StartedAt.Before(*graph[j].StartedAt)
	})
	for idx := range graph {
		for _, manifest := range graph[idx].Manifests {
			serialized, err := yaml.Marshal(manifest)
			if err != nil {
				logrus.WithError(err).Error("Failed to marshal manifest")
				continue
			}
			graph[idx].ManifestsYAML = append(graph[idx].ManifestsYAML, string(serialized))
		}
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "body", graph); err != nil {
		logrus.WithError(err).Error("Error executing template.")
	}

	return buf.String()
}

type Step struct {
	citoolsapi.CIOperatorStepDetails `json:",inline"`
	ManifestsYAML                    []string
}
