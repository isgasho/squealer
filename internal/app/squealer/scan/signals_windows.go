// +build windows

package scan

import (
	log "github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func (s *gitScanner) monitorSignals(processes int, wg sync.WaitGroup) {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		for _ = range c {
			log.Info("Shutting down workers and exiting...")
			for i := 0; i < processes; i++ {
				wg.Done()
			}
			os.Exit(0)
		}
	}()
}
