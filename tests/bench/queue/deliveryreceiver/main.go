package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	runReceiverAndExit(os.Args[1:], func(server *http.Server) error {
		return server.ListenAndServe()
	}, log.Fatalf)
}

// runReceiver parses the receiver's CLI configuration and starts its server.
// listen is injectable so startup configuration can be tested without binding
// a port or running a long-lived server.
func runReceiver(args []string, listen func(*http.Server) error) error {
	flags := flag.NewFlagSet("deliveryreceiver", flag.ContinueOnError)
	addr := flags.String("addr", ":8080", "listen address")
	host := flags.String("host", "", "Host header expected by the signed requests")
	keysPath := flags.String("keys", "", "optional JSON file mapping HTTP Signature key IDs to public PEMs")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return fmt.Errorf("parse flags: %w", err)
	}

	var publicKeys map[string]string
	if *keysPath != "" {
		keysFile, err := os.Open(*keysPath)
		if err != nil {
			return fmt.Errorf("open public keys: %w", err)
		}
		err = json.NewDecoder(keysFile).Decode(&publicKeys)
		closeErr := keysFile.Close()
		if err != nil {
			return fmt.Errorf("decode public keys: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("close public keys: %w", closeErr)
		}
	}

	r, err := newReceiver(config{host: *host, publicKeys: publicKeys})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *addr, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("delivery receiver listening on %s", *addr)
	return listen(srv)
}

func runReceiverAndExit(args []string, listen func(*http.Server) error, fatalf func(string, ...any)) {
	if err := runReceiver(args, listen); err != nil && err != http.ErrServerClosed {
		fatalf("serve: %v", err)
	}
}
