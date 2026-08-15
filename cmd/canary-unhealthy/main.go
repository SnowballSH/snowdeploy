// Command canary-unhealthy is a deliberately broken service, published so a
// deployment can be rehearsed against a failure that is realistic rather than
// convenient.
//
// It starts cleanly, binds its port, and stays up — so the container runtime
// and the unit both report success — and then answers 500 to every request,
// including the health probe. That is the failure worth rehearsing: "started"
// and "healthy" are different claims, and a deploy layer that conflates them
// will happily roll a broken image into production.
//
// Deploying its digest to a scratch manifest should make the daemon probe,
// fail at the deadline, roll back to the previous digest, open a revert pull
// request, and journal the outcome.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// %q escapes the request line, so a crafted path cannot forge log
		// entries by smuggling newlines through this fixture's own output.
		log.Printf("refusing %q %q with 500 (this image is a deploy-failure fixture)",
			r.Method, r.URL.Path)
		http.Error(w, "canary-unhealthy: this service never becomes healthy",
			http.StatusInternalServerError)
	})

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Fprintf(os.Stdout, "canary-unhealthy listening on :%s; every request answers 500\n", port)
	log.Fatal(server.ListenAndServe())
}
