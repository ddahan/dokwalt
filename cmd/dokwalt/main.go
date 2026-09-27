// Command dokwalt deploys Docker Compose apps to your own server.
package main

import (
	"os"

	"github.com/dokwalt/dokwalt/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
