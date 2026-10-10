package main

import (
	"context"
	"fmt"
	"time"

	"github.com/unix/ugh/internal/provider"
	"github.com/unix/ugh/internal/provstate"
	usagerpt "github.com/unix/ugh/internal/usage"
)

// usageCmd prints subscription and credit usage per provider.
func usageCmd([]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	secs, errs := usagerpt.Fetch(ctx, provider.UsageSources())
	fmt.Println(usagerpt.Render(secs, errs, time.Now(), nil))
	if p := provstate.Summary(); p != "" {
		fmt.Println("\nRouting restrictions: " + p)
	}
	if len(secs) == 0 {
		return fmt.Errorf("no usage available")
	}
	return nil
}
