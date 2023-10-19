package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	time.AfterFunc(10*time.Hour, shutdownOfLastResort)
	if os.Getenv("EXIT_ON_START") == "1" {
		return
	}

	fmt.Println("tb running.")

	m := http.NewServeMux()
	m.HandleFunc("/", handle)
	m.HandleFunc("/status", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "OK\n")
	}))
	m.HandleFunc("/quitquitquit", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		os.Exit(0)
	}))
	m.HandleFunc("/test/", test)
	m.HandleFunc("/env", env)
	log.Fatal(http.ListenAndServe(":8080", m))
}

func shutdownOfLastResort() {
	log.Fatalf("shutdown of last resort")
}

func handle(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "hi from fly machines\n")
}

func test(w http.ResponseWriter, r *http.Request) {
	pkg := strings.TrimPrefix(r.RequestURI, "/test/")
	log.Printf("testing %v ...", pkg)
	t0 := time.Now()
	cmd := exec.Command("go", "test", "-json", "-v", pkg)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	d := time.Since(t0).Round(time.Millisecond)
	log.Printf("test of %v = %v in %v", pkg, err, d)
}

func env(w http.ResponseWriter, r *http.Request) {
	j, _ := json.MarshalIndent(os.Environ(), "", "  ")
	w.Write(j)
}
