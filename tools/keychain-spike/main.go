//go:build darwin && cgo

// keychain-spike is an opt-in helper for checking whether two different local
// binary artifacts can access the same unique jira-cli Keychain item.
package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"os"
	"time"

	"github.com/abigotado/jira-cli/internal/auth"
)

var artifactID = "development"

func main() {
	mode := flag.String("mode", "", "write, read, or delete")
	account := flag.String("account", "", "unique spike account")
	sentinel := flag.String("sentinel", "", "unique non-secret sentinel")
	flag.Parse()
	if artifactID == "" || *mode == "" || *account == "" {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := auth.KeychainStore{}
	var err error
	switch *mode {
	case "write":
		err = store.Save(ctx, *account, auth.Credential{Token: *sentinel})
	case "read":
		var credential auth.Credential
		credential, err = store.Load(ctx, *account)
		if err == nil && subtle.ConstantTimeCompare([]byte(credential.Token), []byte(*sentinel)) != 1 {
			os.Exit(1)
		}
	case "delete":
		err = store.Delete(ctx, *account)
	default:
		os.Exit(2)
	}
	if err != nil {
		// Deliberately emit no error text: platform errors are represented only
		// by the exit status so no future wrapping can expose the sentinel.
		os.Exit(1)
	}
}
