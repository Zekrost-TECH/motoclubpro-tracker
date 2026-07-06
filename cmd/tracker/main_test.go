package main

import (
	"context"
	"errors"
	"testing"

	"github.com/DeijoseDevelop/biker-os-tracker/internal/auth"
)

func TestEventKeys(t *testing.T) {
	if got := eventMembersKey("evt-1"); got != "event:evt-1:members" {
		t.Errorf("eventMembersKey = %s, want event:evt-1:members", got)
	}
	if got := eventClubKey("evt-1"); got != "event:evt-1:club" {
		t.Errorf("eventClubKey = %s, want event:evt-1:club", got)
	}
}

func TestTrackKey(t *testing.T) {
	if got := trackKey("evt-1", "user-1"); got != "track:evt-1:user-1" {
		t.Errorf("trackKey = %s, want track:evt-1:user-1", got)
	}
}

func TestTrackPattern(t *testing.T) {
	if got := trackPattern("evt-1"); got != "track:evt-1:*" {
		t.Errorf("trackPattern = %s, want track:evt-1:*", got)
	}
}

func TestAuthorize(t *testing.T) {
	claims := &auth.Claims{
		Clubs: []auth.Club{
			{ClubID: "club-1", Role: "admin"},
			{ClubID: "club-2", Role: "rider"},
		},
	}

	t.Run("member matches", func(t *testing.T) {
		isMember := func(_ context.Context, _ string, _ interface{}) (bool, error) { return true, nil }
		getClub := func(_ context.Context, _ string) (string, error) { return "", errors.New("noop") }
		if !authorize(isMember, getClub, "evt-1", "user-1", claims) {
			t.Fatal("expected true when user is member")
		}
	})

	t.Run("admin of owning club", func(t *testing.T) {
		isMember := func(_ context.Context, _ string, _ interface{}) (bool, error) { return false, nil }
		getClub := func(_ context.Context, _ string) (string, error) { return "club-1", nil }
		if !authorize(isMember, getClub, "evt-1", "user-1", claims) {
			t.Fatal("expected true when user is admin of owning club")
		}
	})

	t.Run("leader of owning club", func(t *testing.T) {
		leaderClaims := &auth.Claims{
			Clubs: []auth.Club{{ClubID: "club-1", Role: "leader"}},
		}
		isMember := func(_ context.Context, _ string, _ interface{}) (bool, error) { return false, nil }
		getClub := func(_ context.Context, _ string) (string, error) { return "club-1", nil }
		if !authorize(isMember, getClub, "evt-1", "user-1", leaderClaims) {
			t.Fatal("expected true when user is leader of owning club")
		}
	})

	t.Run("rider of owning club (not manager)", func(t *testing.T) {
		isMember := func(_ context.Context, _ string, _ interface{}) (bool, error) { return false, nil }
		getClub := func(_ context.Context, _ string) (string, error) { return "club-2", nil }
		if authorize(isMember, getClub, "evt-1", "user-1", claims) {
			t.Fatal("expected false when user is only rider of owning club")
		}
	})

	t.Run("not member and not manager", func(t *testing.T) {
		isMember := func(_ context.Context, _ string, _ interface{}) (bool, error) { return false, nil }
		getClub := func(_ context.Context, _ string) (string, error) { return "club-99", nil }
		if authorize(isMember, getClub, "evt-1", "user-1", claims) {
			t.Fatal("expected false when not member and not manager")
		}
	})

	t.Run("redis error on member check falls through to club check", func(t *testing.T) {
		isMember := func(_ context.Context, _ string, _ interface{}) (bool, error) { return false, errors.New("redis down") }
		getClub := func(_ context.Context, _ string) (string, error) { return "club-1", nil }
		if !authorize(isMember, getClub, "evt-1", "user-1", claims) {
			t.Fatal("expected true when club check succeeds despite member error")
		}
	})

	t.Run("redis error on both checks denies", func(t *testing.T) {
		isMember := func(_ context.Context, _ string, _ interface{}) (bool, error) { return false, errors.New("redis down") }
		getClub := func(_ context.Context, _ string) (string, error) { return "", errors.New("redis down") }
		if authorize(isMember, getClub, "evt-1", "user-1", claims) {
			t.Fatal("expected false when both checks fail")
		}
	})
}
