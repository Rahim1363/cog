package serving

import (
	"io"
	"path/filepath"
	"strings"

	"github.com/replicate/cog/pkg/logger"
	"github.com/replicate/cog/pkg/model"
)

type Platform interface {
	Deploy(mod *model.Model, target string, logWriter logger.Logger) (Deployment, error)
}

type Deployment interface {
	RunInference(input *Example, logWriter logger.Logger) (*Result, error)
	Help(logWriter logger.Logger) (*HelpResponse, error)
	Undeploy() error
}

type ExampleValue struct {
	String *string
	File   *string
}

type Example struct {
	Values map[string]ExampleValue
}

func NewExample(keyVals map[string]string) *Example {
	values := map[string]ExampleValue{}
	for key, val := range keyVals {
		val := val
		if strings.HasPrefix(val, "@") {
			val = val[1:]
			values[key] = ExampleValue{File: &val}
		} else {
			values[key] = ExampleValue{String: &val}
		}
	}
	return &Example{Values: values}
}

func NewExampleWithBaseDir(keyVals map[string]string, baseDir string) *Example {
	values := map[string]ExampleValue{}
	for key, val := range keyVals {
		val := val
		if strings.HasPrefix(val, "@") {
			val = filepath.Join(baseDir, val[1:])
			values[key] = ExampleValue{File: &val}
		} else {
			values[key] = ExampleValue{String: &val}
		}
	}
	return &Example{Values: values}
}

type ResultValue struct {
	Buffer   io.Reader
	MimeType string
}

type Result struct {
	Values    map[string]ResultValue
	SetupTime float64
	RunTime   float64
}

type HelpResponse struct {
	Arguments map[string]*model.RunArgument `json:"arguments"`
}