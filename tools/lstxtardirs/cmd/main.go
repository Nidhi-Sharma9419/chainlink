package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/smartcontractkit/chainlink/v2/tools/lstxtardirs"
)

func main() {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	testDir := filepath.Join(wd, "./testdata/scripts")

	dirs := []string{}
	visitor := lstxtardirs.NewVisitor(testDir, func(path string) error {
		dirs = append(dirs, path)
		return nil
	})
	err = visitor.Walk()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(strings.Join(dirs, "\n"))
}
