package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/robertkrimen/otto"
	"tailscale.com/util/must"
)

type Task struct {
	Name   string `json:",omitempty"`
	GOOS   string `json:",omitempty"`
	GOARCH string `json:",omitempty"`
	Race   bool   `json:",omitempty"`

	Action   string   `json:",omitempty"` // "build", "test", "staticcheck"
	Packages []string `json:",omitempty"`
}

func (t *Task) setPlatform(p string) error {
	p, t.Race = strings.CutSuffix(p, "/race")
	var ok bool
	t.GOOS, t.GOARCH, ok = strings.Cut(p, "/")
	if !ok {
		return errors.New("no slash in GOOS/GOARCH platform")
	}
	return nil
}

type GoPackage struct {
	ImportPath string
	HasTests   bool
}

type GenerateTasksEnv struct {
	GetPackages func(platform string) ([]GoPackage, error)
}

func GenerateTasks(js []byte, env GenerateTasksEnv) (tasks []*Task, retErr error) {
	defer func() {
		if e := recover(); e != nil {
			retErr = fmt.Errorf("error running JS: %v", e)
		}
	}()

	vm := otto.New()
	vm.Set("getPackages", func(call otto.FunctionCall) otto.Value {
		platform := call.Argument(0).String()
		var jsPkgs []otto.Value
		if env.GetPackages != nil {
			pkgs, err := env.GetPackages(platform)
			if err != nil {
				panic(err)
			}
			for _, pkg := range pkgs {
				jsPkgs = append(jsPkgs, must.Get(vm.ToValue(map[string]otto.Value{
					"importPath": must.Get(vm.ToValue(pkg.ImportPath)),
					"hasTests":   must.Get(vm.ToValue(pkg.HasTests)),
				})))
			}
		}
		v, err := vm.ToValue(jsPkgs)
		if err != nil {
			panic(err)
		}
		return v
	})

	vm.Set("addTask", func(call otto.FunctionCall) otto.Value {
		t := &Task{
			Action: call.Argument(0).String(),
		}
		switch t.Action {
		case "test", "build", "test-compile", "staticcheck":
			t.Packages = append(t.Packages, call.Argument(2).String())
		default:
			panic(fmt.Sprintf("invalid task type %q", t.Action))
		}
		t.setPlatform(call.Argument(1).String())
		tasks = append(tasks, t)
		return otto.Value{}
	})
	if _, err := vm.Run(string(js)); err != nil {
		return nil, err
	}
	return tasks, nil
}
