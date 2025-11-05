package main

import (
	"flag"
	"log"
	"os"

	"github.com/louisphilipmarcoux/go-load-balancer/backend"
)

func main() {
	port := flag.String("port", "9001", "Port to listen on")
	flag.Parse()

	serverID, _ := os.Hostname()
	if flag.NArg() > 0 {
		serverID = flag.Arg(0)
	}

	if _, err := backend.RunServer(*port, serverID); err != nil {
		log.Fatal(err)
	}

	select {}
}
