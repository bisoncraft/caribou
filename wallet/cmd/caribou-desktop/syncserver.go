// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build cgo

/*
The sync server is a tiny HTTP server that handles synchronization of multiple
instances of caribou-desktop. Any caribou-desktop instance that manages to get a
running webserver will create a sync server and publish the address at specific
file location. On startup, before getting too far, caribou-desktop looks for a
file, reads the address, and attempts to make contact. If contact is made, the
requesting caribou-desktop will exit without error. The receiving caribou-desktop
will reopen the window if it is closed.

The sync server also enables killing a caribou-desktop process that is running
in the background because the wallet is busy.
*/

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/util/encode"
)

const (
	syncFilename = "syncfile"
)

// getSyncAddress: Get the address stored in the syncfile. If the file doesn't
// exist, an empty string is returned with no error.
func getSyncAddress(syncDir string) (addr, killKey string, err error) {
	syncFile := filepath.Join(syncDir, syncFilename)
	b, err := os.ReadFile(syncFile)
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	if len(b) == 0 { // no sync file
		return "", "", nil
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("unknown syncfile format: %q", string(b))
	}
	return lines[0], lines[1], nil
}

// sendKillSignal sends a kill command to any address in the sync file.
func sendKillSignal(syncDir string) {
	addr, killKey, err := getSyncAddress(syncDir)
	if err != nil {
		log.Errorf("Error getting sync address for kill signal: %v", err)
		return
	}
	if addr == "" {
		log.Errorf("No sync address found for kill signal: %v", err)
		return
	}
	// torproxy: loopback kill signal to our own sync server
	resp, err := http.Post("http://"+addr+"/kill", "application/octet-stream", bytes.NewBufferString(killKey))
	if err != nil {
		log.Errorf("Error sending kill signal to sync server: %v", err)
		return
	}
	if resp == nil {
		log.Errorf("nil kill response")
		return
	}
	if resp.StatusCode != http.StatusOK {
		log.Errorf("Unexpected response code from kill signal send: %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
}

// synchronize attempts to determine the state of any other caribou-desktop
// instances that are running. If none are found, startServer will be true.
// If another instance is found, close will be true, and this instance of
// caribou-desktop should exit immediately.
func synchronize(syncDir string, monitor uintptr) (startServer bool, err error) {
	addr, _, err := getSyncAddress(syncDir)
	if err != nil {
		return false, err
	}
	if addr == "" {
		// Just start the server then.
		return true, nil
	}
	// The running instance opens its window on this instance's monitor.
	resp, err := dexnet.Client.Get(fmt.Sprintf("http://%s/?monitor=%d", addr, monitor))
	if err == nil {
		if resp.StatusCode == http.StatusOK {
			// Other instance will open the window.
			return false, nil
		} else {
			return false, fmt.Errorf("sync server unexpected response code %d: %q", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
	}
	// Error will probably be something like "...: no route to host", but we'll
	// just assume that any error means the server is not running.
	return true, nil
}

// runServer runs an instance of the sync server. Received commands are
// communicated via unbuffered channels. Blocking channels are ignored.
func runServer(ctx context.Context, syncDir string, openC chan<- uintptr, killC chan<- os.Signal, netw util.Network) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Errorf("ListenTCP error: %v", err)
		return
	}

	killKey := hex.EncodeToString(encode.RandomBytes(16))

	err = os.WriteFile(filepath.Join(syncDir, syncFilename), []byte(l.Addr().String()+"\n"+killKey), 0600)
	if err != nil {
		log.Errorf("Failed to start the sync server: %v", err)
		return
	}

	srv := &http.Server{
		Addr:        l.Addr().String(),
		Handler:     &syncServer{openC, killC, killKey},
		IdleTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("listen: %s\n", err)
		}
		log.Infof("Sync server off")
	}()

	<-ctx.Done()
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("http.Server Shutdown errored: %v", err)
	}
	wg.Wait()
}

type syncServer struct {
	openC   chan<- uintptr
	killC   chan<- os.Signal
	killKey string
}

func (s *syncServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		w.WriteHeader(http.StatusOK)
		// A monitor the window process cannot use is ignored there, so an
		// absent or malformed value just means the default placement.
		monitor, _ := strconv.ParseUint(r.URL.Query().Get("monitor"), 10, 64)
		select {
		case s.openC <- uintptr(monitor):
			log.Debug("Received window open request")
		default:
			log.Info("Ignored a window reopen request from another instance")
		}
	case "/kill":
		// killKey is a 32-char hex string; this cap only needs to keep an
		// oversized body from forcing an arbitrary-size read before the key
		// comparison, the same shape as readPost's cap in wallet/appserver.
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		b, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "error reading request body", http.StatusBadRequest)
			return
		}
		if s.killKey != string(b) {
			w.WriteHeader(http.StatusUnauthorized)
			log.Warnf("ignoring kill request with incorrect key %q", string(b))
			return
		}
		w.WriteHeader(http.StatusOK)
		select {
		case s.killC <- os.Interrupt:
			log.Info("Kill signal received")
		default:
			log.Warnf("Kill request ignored (blocking channel)")
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		log.Errorf("syncServer received a request with an unknown path %q", r.URL.Path)
	}
}
