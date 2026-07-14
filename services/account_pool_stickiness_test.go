package services

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func testAccountPoolProviders(ids ...int64) []Provider {
	providers := make([]Provider, 0, len(ids))
	for _, id := range ids {
		providers = append(providers, Provider{ID: id, Name: fmt.Sprintf("key-%d", id)})
	}
	return providers
}

func testAccountPoolIdentity(value string) accountPoolRequestIdentity {
	hash := accountPoolIdentityHash(value)
	return accountPoolRequestIdentity{
		lookupHashes:  []string{hash},
		sessionHashes: []string{hash},
	}
}

func testAccountPoolPreviousResponseIdentity(value string) accountPoolRequestIdentity {
	return accountPoolRequestIdentity{lookupHashes: []string{accountPoolIdentityHash(value)}}
}

func TestAccountPoolStickyStoreInterleavesAndUsesMinimumLoad(t *testing.T) {
	store := newAccountPoolStickyStore()
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2, 3, 4, 5, 6)

	for index := 0; index < 20; index++ {
		request := store.begin(scope, testAccountPoolIdentity(fmt.Sprintf("conversation-%d", index)), providers)
		wantProviderID := int64(index%6 + 1)
		if request.providerID != wantProviderID {
			t.Fatalf("request %d assigned provider %d, want %d", index+1, request.providerID, wantProviderID)
		}
	}

	// After 20 sessions, keys 1 and 2 have four bindings, while 3..6 have
	// three. The next identity must use the lowest-load key, not merely cycle.
	request := store.begin(scope, testAccountPoolIdentity("conversation-100"), providers)
	if request.providerID != 3 {
		t.Fatalf("minimum-load provider = %d, want 3", request.providerID)
	}
}

func TestAccountPoolStickyStoreExpiresIdleSessionWithoutSleep(t *testing.T) {
	store := newAccountPoolStickyStore()
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)
	identity := testAccountPoolIdentity("conversation-expiring")

	first := store.begin(scope, identity, providers)
	if first.providerID != 1 {
		t.Fatalf("first provider = %d, want 1", first.providerID)
	}
	now = now.Add(accountPoolStickyIdleTTL)
	second := store.begin(scope, identity, providers)
	if second.providerID != 2 {
		t.Fatalf("expired session provider = %d, want a newly assigned key 2", second.providerID)
	}
}

func TestAccountPoolStickyStoreScopesAreIsolated(t *testing.T) {
	store := newAccountPoolStickyStore()
	scopeA := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	scopeB := newAccountPoolStickyScope("owner-a", "relay-b", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	firstA := store.begin(scopeA, testAccountPoolIdentity("shared-conversation"), providers)
	if firstA.providerID != 1 {
		t.Fatalf("scope A first provider = %d, want 1", firstA.providerID)
	}
	secondA := store.begin(scopeA, testAccountPoolIdentity("different-conversation"), providers)
	if secondA.providerID != 2 {
		t.Fatalf("scope A second provider = %d, want 2", secondA.providerID)
	}
	firstB := store.begin(scopeB, testAccountPoolIdentity("shared-conversation"), providers)
	if firstB.providerID != 1 {
		t.Fatalf("scope B first provider = %d, want 1", firstB.providerID)
	}
	store.mu.Lock()
	distinctScopeState := store.scopes[scopeA] != store.scopes[scopeB]
	store.mu.Unlock()
	if !distinctScopeState {
		t.Fatal("same conversation across relay keys reused sticky scope state")
	}
	secondB := store.begin(scopeB, testAccountPoolIdentity("another-conversation"), providers)
	if secondB.providerID != 2 {
		t.Fatalf("scope B second provider = %d, want 2", secondB.providerID)
	}
}

func TestAccountPoolStickyStoreSharesLoadAcrossRelayKeys(t *testing.T) {
	store := newAccountPoolStickyStore()
	scopeA := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	scopeB := newAccountPoolStickyScope("owner-a", "relay-b", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2, 3)

	for index := 0; index < 12; index++ {
		scope := scopeA
		if index%2 != 0 {
			scope = scopeB
		}
		request := store.begin(scope, testAccountPoolIdentity(fmt.Sprintf("conversation-%d", index)), providers)
		wantProviderID := int64(index%3 + 1)
		if request.providerID != wantProviderID {
			t.Fatalf("request %d assigned provider %d, want %d", index+1, request.providerID, wantProviderID)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	loads := store.loads[scopeA.loadScope()].sessionCounts
	for _, providerID := range []int64{1, 2, 3} {
		if loads[providerID] != 4 {
			t.Fatalf("shared provider %d load = %d, want 4", providerID, loads[providerID])
		}
	}
}

func TestAccountPoolStickyStoreConcurrentNewSessionsShareLoadAcrossRelayKeys(t *testing.T) {
	store := newAccountPoolStickyStore()
	scopeA := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	scopeB := newAccountPoolStickyScope("owner-a", "relay-b", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	const workers = 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			scope := scopeA
			if index%2 != 0 {
				scope = scopeB
			}
			store.begin(scope, testAccountPoolIdentity(fmt.Sprintf("concurrent-%d", index)), providers)
		}(index)
	}
	close(start)
	wg.Wait()

	store.mu.Lock()
	defer store.mu.Unlock()
	loads := store.loads[scopeA.loadScope()].sessionCounts
	if loads[1] != workers/2 || loads[2] != workers/2 {
		t.Fatalf("shared concurrent loads = %#v, want %d each", loads, workers/2)
	}
}

func TestAccountPoolStickyStoreSharesAnonymousReservationsAcrossRelayKeys(t *testing.T) {
	store := newAccountPoolStickyStore()
	scopeA := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	scopeB := newAccountPoolStickyScope("owner-a", "relay-b", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	first := store.begin(scopeA, accountPoolRequestIdentity{}, providers)
	second := store.begin(scopeB, accountPoolRequestIdentity{}, providers)
	if first.providerID != 1 || second.providerID != 2 {
		t.Fatalf("shared reservations assigned (%d,%d), want (1,2)", first.providerID, second.providerID)
	}
	store.abort(first)
	third := store.begin(scopeB, accountPoolRequestIdentity{}, providers)
	if third.providerID != 1 {
		t.Fatalf("reservation release selected provider %d, want 1", third.providerID)
	}

	store.invalidateProvider(scopeA.userID, scopeA.platform, scopeA.poolID, 1)
	store.mu.Lock()
	loadState := store.loads[scopeA.loadScope()]
	if len(loadState.reservations[1]) != 0 {
		store.mu.Unlock()
		t.Fatalf("blacklisted provider retained reservation: %#v", loadState.reservations)
	}
	store.mu.Unlock()
	// A stale request from before blacklisting must not release a reservation
	// created after that provider becomes selectable again.
	fourth := store.begin(scopeA, accountPoolRequestIdentity{}, providers)
	if fourth.providerID != 1 {
		t.Fatalf("post-blacklist reservation selected provider %d, want 1", fourth.providerID)
	}
	store.abort(third)
	store.mu.Lock()
	stillReserved := len(store.loads[scopeA.loadScope()].reservations[1])
	store.mu.Unlock()
	if stillReserved != 1 {
		t.Fatalf("stale reservation abort released newer reservation, count = %d", stillReserved)
	}
	store.abort(second)
	store.abort(fourth)
}

func TestAccountPoolStickyStorePrunesExpiredLoadFromOtherRelayKey(t *testing.T) {
	store := newAccountPoolStickyStore()
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	scopeA := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	scopeB := newAccountPoolStickyScope("owner-a", "relay-b", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	store.begin(scopeA, testAccountPoolIdentity("expired-in-a"), providers)
	now = now.Add(accountPoolStickyIdleTTL)
	store.begin(scopeB, testAccountPoolIdentity("new-in-b"), providers)

	store.mu.Lock()
	defer store.mu.Unlock()
	loads := store.loads[scopeA.loadScope()].sessionCounts
	if total := loads[1] + loads[2]; total != 1 {
		t.Fatalf("expired cross-key session still counted: %#v", loads)
	}
}

func TestAccountPoolStickyStoreConcurrentKnownIdentityUsesOneKey(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)
	identity := testAccountPoolIdentity("conversation-concurrent")

	const workers = 32
	start := make(chan struct{})
	results := make(chan int64, workers)
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- store.begin(scope, identity, providers).providerID
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for providerID := range results {
		if providerID != 1 {
			t.Fatalf("concurrent binding selected provider %d, want 1", providerID)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if sessions := len(store.scopes[scope].sessions); sessions != 1 {
		t.Fatalf("concurrent identity created %d sessions, want 1", sessions)
	}
}

func TestAccountPoolStickyStoreLinksConversationAndResponseAliases(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	conversation := testAccountPoolIdentity("conversation-1")
	first := store.begin(scope, conversation, providers)
	store.commit(first, first.providerID, "resp_A")

	second := store.begin(scope, testAccountPoolPreviousResponseIdentity("resp_A"), providers)
	if second.providerID != first.providerID {
		t.Fatalf("previous_response_id alias selected %d, want %d", second.providerID, first.providerID)
	}
	store.commit(second, second.providerID, "resp_B")

	third := store.begin(scope, testAccountPoolPreviousResponseIdentity("resp_B"), providers)
	if third.providerID != first.providerID {
		t.Fatalf("response alias chain selected %d, want %d", third.providerID, first.providerID)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if sessions := len(store.scopes[scope].sessions); sessions != 1 {
		t.Fatalf("alias chain sessions = %d, want 1", sessions)
	}
}

func TestAccountPoolPromptCacheKeyCreatesDurableSession(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)
	identity := accountPoolRequestIdentityFromBody([]byte(`{"prompt_cache_key":"pi-session-1"}`))
	if identity.provisionalHash != "" || len(identity.sessionHashes) != 1 {
		t.Fatalf("prompt_cache_key identity = %#v, want durable session identity", identity)
	}
	first := store.begin(scope, identity, providers)
	second := store.begin(scope, accountPoolRequestIdentityFromBody([]byte(`{"prompt_cache_key":"pi-session-1"}`)), providers)
	if second.providerID != first.providerID {
		t.Fatalf("prompt_cache_key continuation selected %d, want %d", second.providerID, first.providerID)
	}
}

func TestAccountPoolStickyStoreForgedPreviousResponseIDsDoNotGrowAliases(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	first := store.begin(scope, testAccountPoolIdentity("conversation-owner"), providers)
	if first.providerID != 1 || first.sessionID == "" {
		t.Fatalf("first conversation binding = (%d,%q), want provider 1 with a session", first.providerID, first.sessionID)
	}

	for index := 0; index < 64; index++ {
		identity := accountPoolRequestIdentityFromBody([]byte(fmt.Sprintf(`{"conversation":"conversation-owner","previous_response_id":"forged-response-%d"}`, index)))
		request := store.begin(scope, identity, providers)
		if request.sessionID != first.sessionID || request.providerID != first.providerID {
			t.Fatalf("forged previous response %d changed binding to (%d,%q), want (%d,%q)", index, request.providerID, request.sessionID, first.providerID, first.sessionID)
		}
		store.abort(request)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	state := store.scopes[scope]
	if aliases := len(state.aliases); aliases != 1 {
		t.Fatalf("forged previous responses grew alias map to %d, want 1", aliases)
	}
	if sessions := len(state.sessions); sessions != 1 {
		t.Fatalf("forged previous responses changed session count to %d, want 1", sessions)
	}
}

func TestAccountPoolStickyStoreInboundPreviousAliasCannotMergeSessions(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	conversationA := store.begin(scope, testAccountPoolIdentity("conversation-a"), providers)
	store.commit(conversationA, conversationA.providerID, "response-a")
	conversationB := store.begin(scope, testAccountPoolIdentity("conversation-b"), providers)
	store.commit(conversationB, conversationB.providerID, "response-b")
	if conversationA.providerID == conversationB.providerID {
		t.Fatalf("test requires distinct session keys, got %d", conversationA.providerID)
	}

	// Conversation A wins lookup precedence. A client-provided response ID from
	// B must not be attached to A or cause B's session to be merged away.
	request := store.begin(scope, accountPoolRequestIdentityFromBody([]byte(`{"conversation":"conversation-a","previous_response_id":"response-b"}`)), providers)
	if request.sessionID != conversationA.sessionID || request.providerID != conversationA.providerID {
		t.Fatalf("conversation A with response B selected (%d,%q), want (%d,%q)", request.providerID, request.sessionID, conversationA.providerID, conversationA.sessionID)
	}
	store.commit(request, request.providerID, "response-a-next")

	continuationB := store.begin(scope, testAccountPoolPreviousResponseIdentity("response-b"), providers)
	if continuationB.sessionID != conversationB.sessionID || continuationB.providerID != conversationB.providerID {
		t.Fatalf("response B continuation selected (%d,%q), want (%d,%q)", continuationB.providerID, continuationB.sessionID, conversationB.providerID, conversationB.sessionID)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	state := store.scopes[scope]
	if sessions := len(state.sessions); sessions != 2 {
		t.Fatalf("inbound response alias merged sessions to %d, want 2", sessions)
	}
	if mapped := state.aliases[accountPoolIdentityHash("response-b")]; mapped != conversationB.sessionID {
		t.Fatalf("response B alias mapped to %q, want %q", mapped, conversationB.sessionID)
	}
}

func TestAccountPoolStickyStoreSuccessfulResponseIDSupportsContinuation(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	initial := store.begin(scope, accountPoolRequestIdentity{}, providers)
	if !initial.holdsReservation {
		t.Fatal("anonymous request should hold a provisional reservation")
	}
	store.commit(initial, initial.providerID, "response-from-upstream")

	continuation := store.begin(scope, accountPoolRequestIdentityFromBody([]byte(`{"previous_response_id":"response-from-upstream"}`)), providers)
	if continuation.sessionID != initial.sessionID || continuation.providerID != initial.providerID {
		t.Fatalf("successful response continuation selected (%d,%q), want (%d,%q)", continuation.providerID, continuation.sessionID, initial.providerID, initial.sessionID)
	}
}

func TestAccountPoolStickyStoreAnonymousRequestNeedsResponseID(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)

	anonymous := store.begin(scope, accountPoolRequestIdentity{}, providers)
	store.abort(anonymous)
	store.mu.Lock()
	if sessions := len(store.scopes[scope].sessions); sessions != 0 {
		store.mu.Unlock()
		t.Fatalf("anonymous failed request left %d sessions", sessions)
	}
	store.mu.Unlock()

	anonymous = store.begin(scope, accountPoolRequestIdentity{}, providers)
	store.commit(anonymous, anonymous.providerID, "resp-linked")
	linked := store.begin(scope, testAccountPoolPreviousResponseIdentity("resp-linked"), providers)
	if linked.providerID != anonymous.providerID {
		t.Fatalf("response-id-linked anonymous session selected %d, want %d", linked.providerID, anonymous.providerID)
	}
}

func TestAccountPoolStickyStoreKeepsTransientFallbackAndReassignsInvalidBinding(t *testing.T) {
	store := newAccountPoolStickyStore()
	scope := newAccountPoolStickyScope("owner-a", "relay-a", "openai-responses", "pool-a")
	providers := testAccountPoolProviders(1, 2)
	identity := testAccountPoolIdentity("conversation-reassign")

	first := store.begin(scope, identity, providers)
	if first.providerID != 1 {
		t.Fatalf("first provider = %d, want 1", first.providerID)
	}
	// A successful same-request fallback must not migrate a valid sticky key.
	store.commit(first, 2, "resp-fallback")
	stillBound := store.begin(scope, identity, providers)
	if stillBound.providerID != 1 {
		t.Fatalf("transient fallback migrated session to %d, want 1", stillBound.providerID)
	}

	store.invalidateProvider(scope.userID, scope.platform, scope.poolID, 1)
	blacklistReassigned := store.begin(scope, identity, providers)
	if blacklistReassigned.providerID != 2 {
		t.Fatalf("blacklisted binding reassigned to %d, want 2", blacklistReassigned.providerID)
	}

	store = newAccountPoolStickyStore()
	first = store.begin(scope, identity, providers)
	deletedReassigned := store.begin(scope, identity, testAccountPoolProviders(2))
	if first.providerID != 1 || deletedReassigned.providerID != 2 {
		t.Fatalf("deleted binding reassign = (%d,%d), want (1,2)", first.providerID, deletedReassigned.providerID)
	}
}
