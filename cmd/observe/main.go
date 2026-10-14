package main

import (
	"context"
	"fmt"
	"github.com/owenyeo/go-observe/internal/cli"
	"os"
)

func main() {
	if err := cli.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "observe:", err)
		os.Exit(1)
	}
}
