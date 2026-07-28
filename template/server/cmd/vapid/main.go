// Command vapid generates a fresh VAPID key pair for web push and prints it in
// .env format. Run once, then paste the values into your environment.
package main

import (
	"fmt"
	"log"

	"github.com/robert-crandall-org/go-home-server/notify"
)

func main() {
	public, private, err := notify.GenerateVAPIDKeys()
	if err != nil {
		log.Fatalf("generate vapid keys: %v", err)
	}
	fmt.Printf("VAPID_PUBLIC_KEY=%s\n", public)
	fmt.Printf("VAPID_PRIVATE_KEY=%s\n", private)
}
