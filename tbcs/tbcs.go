// The tbcs command is the TB cache server. It's a stateless, sharded service.
// It exists to work around spread network load between machines, as with
// only one cache server, we quickly hit network bandwidth limits.
package main

import (
	"io"
	"log"
	"net/http"
)

func main() {
	log.Printf("cache server running.")
	log.Fatal(http.ListenAndServe(":8080", http.HandlerFunc(serve)))
}

func serve(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "hi from tbcs\n")
}
