// Command node runs one defichain node: the block chain, txpool, p2p, miner, JSON-RPC
// and admin API, all on one HTTP port.
//
//	go run ./cmd/node -genesis genesis.json -config node1.json
//
// genesis.json is the network's rules and block 0 (identical on every node); node1.json is
// this node's own settings (id, listen address, peers, coinbase, mining). See spec/spec.go
// for both formats, or let `go run ./cmd/devnet` write them for you.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"defichain/chain"
	"defichain/spec"
)

func main() {
	genesisPath := flag.String("genesis", "genesis.json", "network genesis file")
	configPath := flag.String("config", "", "node config file (required)")
	parentWatch := flag.Bool("parent-watch", false, "exit when stdin closes (set by the devnet launcher, so nodes die with it)")
	flag.Parse()
	if *parentWatch {
		go func() {
			io.Copy(io.Discard, os.Stdin) //nolint:errcheck // returns when the launcher's end of the pipe closes
			os.Exit(0)
		}()
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: node -genesis genesis.json -config node.json")
		os.Exit(2)
	}
	if err := run(*genesisPath, *configPath); err != nil {
		fmt.Fprintf(os.Stderr, "node: %v\n", err)
		os.Exit(1)
	}
}

func run(genesisPath, configPath string) error {
	g, err := spec.LoadGenesis(genesisPath)
	if err != nil {
		return err
	}
	cfg, err := spec.LoadNodeConfig(configPath)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "["+cfg.ID+"] ", log.Ltime|log.Lmicroseconds)
	n, err := chain.NewNode(*cfg, g, logger)
	if err != nil {
		return err
	}
	if err := n.Start(); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	logger.Printf("shutting down")
	n.Close()
	return nil
}
