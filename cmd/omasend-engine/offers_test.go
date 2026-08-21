package main

import (
	"context"
	"testing"

	"omasend/internal/server"
)

// Parked offers must be bounded: past maxHeldOffers a new offer is declined
// immediately instead of retained, so a remote peer cannot grow the offer
// map (and the metadata each entry pins) without limit.
func TestHoldOfferCapsParkedOffers(t *testing.T) {
	e := &engine{hub: newHub(), offers: map[string]server.AcceptRequest{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	replies := make([]chan server.AcceptDecision, 0, maxHeldOffers+3)
	for i := 0; i < maxHeldOffers+3; i++ {
		reply := make(chan server.AcceptDecision, 1)
		replies = append(replies, reply)
		e.holdOffer(ctx, server.AcceptRequest{Reply: reply})
	}

	e.offerMu.Lock()
	held := len(e.offers)
	e.offerMu.Unlock()
	if held != maxHeldOffers {
		t.Fatalf("parked offers = %d, want cap %d", held, maxHeldOffers)
	}
	for i, reply := range replies {
		select {
		case d := <-reply:
			if i < maxHeldOffers {
				t.Fatalf("offer %d resolved early (accept=%v) — should still be parked", i, d.Accept)
			}
			if d.Accept {
				t.Fatalf("over-cap offer %d was accepted", i)
			}
		default:
			if i >= maxHeldOffers {
				t.Fatalf("over-cap offer %d was parked instead of declined", i)
			}
		}
	}
}
