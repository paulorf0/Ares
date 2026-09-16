package main

import (
	"bufio"
	"log/slog"
	"os"
)

// readInput streams stdin to the returned channel, one line per Enter. The
// channel is closed on EOF.
func readInput() <-chan string {
	lines := make(chan string)

	go func() {
		defer close(lines)

		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		// Scan stops on both EOF and a read error; only the latter sets Err.
		if err := scanner.Err(); err != nil {
			slog.Error("read stdin", "error", err)
		}
	}()

	return lines
}
