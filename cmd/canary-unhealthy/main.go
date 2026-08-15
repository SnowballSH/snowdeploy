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
	"sync/atomic"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Nothing from the request is logged. This fixture answers every path
	// identically, so echoing the request line would add no information while
	// letting a crafted path forge entries in its own output.
	var refused atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		log.Printf("refused request %d with 500 (this image is a deploy-failure fixture)",
			refused.Add(1))
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
