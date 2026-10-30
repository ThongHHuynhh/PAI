// keygen prints a new release key pair: the private half for the
// RELEASE_SIGNING_KEY secret, the public half for the RELEASE_PUBLIC_KEY
// variable, both base64. Run it once, on a machine you trust.
//
//	go run ./pkg/release/keygen
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("RELEASE_SIGNING_KEY (secret):  ", base64.StdEncoding.EncodeToString(priv.Seed()))
	fmt.Println("RELEASE_PUBLIC_KEY (variable): ", base64.StdEncoding.EncodeToString(pub))
}
