package interceptor

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/auth"
)

func TestRateLimit(t *testing.T) {
	if NewRateLimit(0, 5, nil) != nil {
		t.Fatal("rate 0 must disable")
	}
	r := NewRateLimit(2, 3, nil)
	now := time.Unix(0, 0)
	r.now = func() time.Time { return now }
	ann := auth.WithIdentity(context.Background(), auth.Identity{Subject: "ann", Source: auth.SourceLocal})
	bob := auth.WithIdentity(context.Background(), auth.Identity{Subject: "bob", Source: auth.SourceLocal})
	for i := range 3 {
		if err := r.check(ann, "/x"); err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
	}
	if err := r.check(ann, "/x"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over burst: %v", err)
	}
	if err := r.check(bob, "/x"); err != nil { // bucket per user
		t.Fatal(err)
	}
	if err := r.check(ann, healthPrefix+"Check"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(500 * time.Millisecond) // +1 token
	if r.check(ann, "/x") != nil || r.check(ann, "/x") == nil {
		t.Fatal("refill")
	}
	if err := r.check(context.Background(), "/x"); status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	var off *RateLimit
	if off.check(ann, "/x") != nil {
		t.Fatal("nil limiter")
	}
}
