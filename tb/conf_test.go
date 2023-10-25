package main

import (
	"encoding/json"
	"os"
	"testing"

	"tailscale.com/util/must"
)

func TestGenerateTasks(t *testing.T) {
	env := GenerateTasksEnv{
		GetPackages: func(platform string) ([]GoPackage, error) {
			return []GoPackage{
				{ImportPath: "./util/foo", HasTests: true},
				{ImportPath: "./util/bar", HasTests: true},
				{ImportPath: "./tempfork/spoon", HasTests: true},
				{ImportPath: "./cmd/notests", HasTests: false},
			}, nil
		},
	}
	got, err := GenerateTasks(must.Get(os.ReadFile("gen1_test.js")), env)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.MarshalIndent(got, "", "\t")
	t.Logf("got: %s", j)
}
