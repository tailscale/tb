package main

import (
	"fmt"

	"github.com/robertkrimen/otto"
	"github.com/tailscale/tb/tb/tbtype"
	"tailscale.com/util/must"
)

type GoPackage struct {
	ImportPath string
	HasTests   bool
}

type GenerateTasksEnv struct {
	GetPackages func(platform string) ([]GoPackage, error)
}

func GenerateTasks(js []byte, env GenerateTasksEnv) (tasks []*tbtype.Task, retErr error) {
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
		t := &tbtype.Task{
			Action: call.Argument(0).String(),
		}
		switch t.Action {
		case "test", "build", "test-compile", "staticcheck":
			t.Packages = append(t.Packages, call.Argument(2).String())
		default:
			panic(fmt.Sprintf("invalid task type %q", t.Action))
		}
		t.SetPlatform(call.Argument(1).String())
		tasks = append(tasks, t)
		return otto.Value{}
	})
	if _, err := vm.Run(string(js)); err != nil {
		return nil, err
	}
	return tasks, nil
}
