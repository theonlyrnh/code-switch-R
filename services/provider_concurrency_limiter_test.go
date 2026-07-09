package services

import (
	"testing"
	"time"
)

func TestProviderConcurrencyLimiterAcquireRelease(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 42, MaxConcurrency: 1}

	release, ok := limiter.TryAcquire("user-a", "claude", provider)
	if !ok || release == nil {
		t.Fatal("first acquire should succeed")
	}
	if _, ok := limiter.TryAcquire("user-a", "claude", provider); ok {
		t.Fatal("second acquire should fail at max concurrency")
	}

	release()
	release()

	releaseAgain, ok := limiter.TryAcquire("user-a", "claude", provider)
	if !ok || releaseAgain == nil {
		t.Fatal("acquire after idempotent release should succeed")
	}
	releaseAgain()
}

func TestProviderConcurrencyLimiterDefaultAndZeroConcurrencyAreUnlimitedForPracticalUse(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providers := []Provider{
		{ID: 1},
		{ID: 2, MaxConcurrency: 0},
		{ID: 3, MaxConcurrency: defaultProviderMaxConcurrency},
	}

	for _, provider := range providers {
		for i := 0; i < 3; i++ {
			if release, ok := limiter.TryAcquire("", "openai-chat", provider); !ok || release == nil {
				t.Fatalf("provider %#v acquire %d should succeed", provider, i)
			}
		}
	}
}

func TestProviderConcurrencyLimiterQueueFIFOAndRemove(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	first := &ProviderQueueItem{RequestID: 1, UserID: "user-a", Platform: "claude", PoolID: "pool-a"}
	second := &ProviderQueueItem{RequestID: 2, UserID: "user-a", Platform: "claude", PoolID: "pool-a"}
	third := &ProviderQueueItem{RequestID: 3, UserID: "user-a", Platform: "claude", PoolID: "pool-a"}

	if position := limiter.Enqueue(first); position != 1 {
		t.Fatalf("first position = %d, want 1", position)
	}
	if position := limiter.Enqueue(second); position != 2 {
		t.Fatalf("second position = %d, want 2", position)
	}
	if position := limiter.Enqueue(third); position != 3 {
		t.Fatalf("third position = %d, want 3", position)
	}

	if !limiter.RemoveQueued(2) {
		t.Fatal("expected queued request 2 to be removed")
	}
	if position, ok := limiter.QueuePosition(3); !ok || position != 2 {
		t.Fatalf("third position after remove = (%d,%v), want (2,true)", position, ok)
	}
}

func TestProviderConcurrencyLimiterWakeQueue(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	first := &ProviderQueueItem{RequestID: 1, Platform: "openai-responses", PoolID: "pool-a"}
	second := &ProviderQueueItem{RequestID: 2, Platform: "openai-responses", PoolID: "pool-a"}
	queueKey := providerQueueKey("", "openai-responses", "pool-a")

	limiter.Enqueue(first)
	limiter.Enqueue(second)
	limiter.WakeQueue("", "openai-responses", "pool-a")

	select {
	case <-first.Ready:
	default:
		t.Fatal("first queued request should be woken")
	}
	if _, ok := limiter.QueuePosition(1); ok {
		t.Fatal("woken request should no longer have a queue position")
	}
	if !limiter.HasQueueAhead(queueKey, 3) {
		t.Fatal("new request should see existing queue/woken request ahead")
	}
	limiter.CompleteWake(queueKey, 1)
	if !limiter.HasQueueAhead(queueKey, 3) {
		t.Fatal("new request should still see second queued request ahead")
	}

	limiter.WakeQueue("", "openai-responses", "pool-a")
	select {
	case <-second.Ready:
	default:
		t.Fatal("second queued request should be woken")
	}
}

func TestProviderConcurrencyLimiterWakeQueuesForPlatform(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	poolA := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a"}
	poolB := &ProviderQueueItem{RequestID: 2, Platform: "openai-chat", PoolID: "pool-b"}
	otherPlatform := &ProviderQueueItem{RequestID: 3, Platform: "claude", PoolID: "pool-c"}

	limiter.Enqueue(poolA)
	limiter.Enqueue(poolB)
	limiter.Enqueue(otherPlatform)

	keys := limiter.WakeQueuesForPlatform("", "openai-chat")
	if len(keys) != 2 {
		t.Fatalf("woken queue keys = %d, want 2", len(keys))
	}
	for _, item := range []*ProviderQueueItem{poolA, poolB} {
		select {
		case <-item.Ready:
		default:
			t.Fatalf("request %d should be woken", item.RequestID)
		}
	}
	select {
	case <-otherPlatform.Ready:
		t.Fatal("other platform queue should not be woken")
	default:
	}
}

func TestProviderConcurrencyLimiterWakeHandoffAfterCompleteWake(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	first := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a"}
	second := &ProviderQueueItem{RequestID: 2, Platform: "openai-chat", PoolID: "pool-a"}
	queueKey := providerQueueKey("", "openai-chat", "pool-a")

	limiter.Enqueue(first)
	limiter.Enqueue(second)
	limiter.WakeQueue("", "openai-chat", "pool-a")
	limiter.WakeQueue("", "openai-chat", "pool-a")

	select {
	case <-first.Ready:
	default:
		t.Fatal("first queued request should be woken")
	}
	select {
	case <-second.Ready:
		t.Fatal("second should not wake while first is still marked woken")
	default:
	}

	limiter.CompleteWake(queueKey, first.RequestID)
	limiter.WakeQueuesForPlatform("", "openai-chat")
	select {
	case <-second.Ready:
	default:
		t.Fatal("second queued request should wake after first completes wake")
	}
}

func TestProviderConcurrencyLimiterRemoveWokenReportsStatusForHandoff(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	first := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a"}
	second := &ProviderQueueItem{RequestID: 2, Platform: "openai-chat", PoolID: "pool-a"}

	limiter.Enqueue(first)
	limiter.Enqueue(second)
	limiter.WakeQueue("", "openai-chat", "pool-a")

	removed, wasWoken := limiter.RemoveQueuedWithStatus(first.RequestID)
	if !removed || !wasWoken {
		t.Fatalf("RemoveQueuedWithStatus = (%v,%v), want (true,true)", removed, wasWoken)
	}

	limiter.WakeQueuesForPlatform("", "openai-chat")
	select {
	case <-second.Ready:
	default:
		t.Fatal("second queued request should wake after woken first is removed")
	}
}

func TestProviderConcurrencyLimiterWakeQueueForProviderWakesOldestMatchingHead(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerOne := Provider{ID: 1, MaxConcurrency: 1}
	providerTwo := Provider{ID: 2, MaxConcurrency: 1}
	oldestForProviderOne := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{providerOne}, ProviderIDs: []int64{1}}
	newerForProviderOne := &ProviderQueueItem{RequestID: 2, Platform: "openai-chat", PoolID: "pool-b", Providers: []Provider{providerOne}, ProviderIDs: []int64{1}}
	otherProvider := &ProviderQueueItem{RequestID: 3, Platform: "openai-chat", PoolID: "pool-c", Providers: []Provider{providerTwo}, ProviderIDs: []int64{2}}

	limiter.Enqueue(oldestForProviderOne)
	time.Sleep(time.Millisecond)
	limiter.Enqueue(newerForProviderOne)
	time.Sleep(time.Millisecond)
	limiter.Enqueue(otherProvider)

	keys := limiter.WakeQueueForProvider("", "openai-chat", providerOne)
	if len(keys) != 1 || keys[0] != providerQueueKey("", "openai-chat", "pool-a") {
		t.Fatalf("woken keys = %v, want only pool-a", keys)
	}
	select {
	case <-oldestForProviderOne.Ready:
	default:
		t.Fatal("oldest matching provider queue should be woken")
	}
	select {
	case <-newerForProviderOne.Ready:
		t.Fatal("newer matching provider queue should not wake while older matching queue is woken")
	default:
	}
	select {
	case <-otherProvider.Ready:
		t.Fatal("different provider queue should not be woken")
	default:
	}

	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-a"), oldestForProviderOne.RequestID)
	keys = limiter.WakeQueueForProvider("", "openai-chat", providerOne)
	if len(keys) != 1 || keys[0] != providerQueueKey("", "openai-chat", "pool-b") {
		t.Fatalf("second woken keys = %v, want only pool-b", keys)
	}
	select {
	case <-newerForProviderOne.Ready:
	default:
		t.Fatal("newer matching provider queue should wake after older completes")
	}
}

func TestProviderConcurrencyLimiterRemoveHeadExposesProviderHandoff(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerOne := Provider{ID: 1, MaxConcurrency: 1}
	providerTwo := Provider{ID: 2, MaxConcurrency: 1}
	providerTwoOnly := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{providerTwo}, ProviderIDs: []int64{2}}
	providerOneNext := &ProviderQueueItem{RequestID: 2, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{providerOne}, ProviderIDs: []int64{1}}

	limiter.Enqueue(providerTwoOnly)
	limiter.Enqueue(providerOneNext)

	if keys := limiter.WakeQueueForProvider("", "openai-chat", providerOne); len(keys) != 0 {
		t.Fatalf("provider 1 should not wake while provider 2-only item is queue head, got %v", keys)
	}

	removed, wasWoken, providerID, exposedProviders, exposedHead := limiter.RemoveQueuedWithWakeInfo(providerTwoOnly.RequestID)
	if !removed || wasWoken || providerID != 0 || !exposedHead {
		t.Fatalf("RemoveQueuedWithWakeInfo = (%v,%v,%d,%v,%v), want removed non-woken exposed head", removed, wasWoken, providerID, exposedProviders, exposedHead)
	}
	if len(exposedProviders) != 1 || exposedProviders[0].ID != 1 {
		t.Fatalf("exposed providers = %v, want provider 1", exposedProviders)
	}

	for _, provider := range exposedProviders {
		limiter.WakeQueueForProvider("", "openai-chat", provider)
	}
	select {
	case <-providerOneNext.Ready:
	default:
		t.Fatal("provider 1-capable item should wake after provider 2-only head is removed")
	}
}

func TestProviderConcurrencyLimiterWokenProviderReservationBlocksNewRequests(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}
	item := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{provider}, ProviderIDs: []int64{1}}

	limiter.Enqueue(item)
	keys := limiter.WakeQueueForProvider("", "openai-chat", provider)
	if len(keys) != 1 {
		t.Fatalf("woken keys = %v, want one key", keys)
	}

	if release, ok := limiter.TryAcquire("", "openai-chat", provider); ok {
		release()
		t.Fatal("new request should not acquire provider reserved for woken queued request")
	}

	release, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, item.RequestID)
	if !ok || release == nil {
		t.Fatal("woken request should acquire its reserved provider slot")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-a"), item.RequestID)
	release(false)
}

func TestProviderConcurrencyLimiterReservedRequestCannotAcquireDifferentProvider(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	reservedProvider := Provider{ID: 1, MaxConcurrency: 1}
	otherProvider := Provider{ID: 2, MaxConcurrency: 1}
	item := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{reservedProvider}, ProviderIDs: []int64{1}}

	limiter.Enqueue(item)
	if keys := limiter.WakeQueueForProvider("", "openai-chat", reservedProvider); len(keys) != 1 {
		t.Fatalf("wake keys = %v, want one key", keys)
	}
	if providerID, ok := limiter.ReservedProviderForRequest(item.RequestID); !ok || providerID != reservedProvider.ID {
		t.Fatalf("ReservedProviderForRequest = (%d,%v), want (%d,true)", providerID, ok, reservedProvider.ID)
	}
	if release, ok := limiter.TryAcquireForRequest("", "openai-chat", otherProvider, item.RequestID); ok {
		release(false)
		t.Fatal("reserved request should not acquire a different provider")
	}
	if release, ok := limiter.TryAcquireForRequest("", "openai-chat", reservedProvider, item.RequestID); !ok || release == nil {
		t.Fatal("reserved request should acquire reserved provider")
	} else {
		limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-a"), item.RequestID)
		release(false)
	}
}

func TestProviderConcurrencyLimiterWakeQueueForProviderHonorsPendingReservations(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}
	first := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{provider}, ProviderIDs: []int64{1}}
	second := &ProviderQueueItem{RequestID: 2, Platform: "openai-chat", PoolID: "pool-b", Providers: []Provider{provider}, ProviderIDs: []int64{1}}

	limiter.Enqueue(first)
	limiter.Enqueue(second)
	if keys := limiter.WakeQueueForProvider("", "openai-chat", provider); len(keys) != 1 {
		t.Fatalf("first wake keys = %v, want one key", keys)
	}
	if keys := limiter.WakeQueueForProvider("", "openai-chat", provider); len(keys) != 0 {
		t.Fatalf("second wake should not create another reservation at limit 1, got %v", keys)
	}
	select {
	case <-second.Ready:
		t.Fatal("second request should not be woken while first reservation is pending")
	default:
	}
}

func TestProviderConcurrencyLimiterReleaseWakeCreatesReservationAtomically(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}
	queued := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{provider}, ProviderIDs: []int64{1}}

	release, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, 10)
	if !ok || release == nil {
		t.Fatal("initial acquire should succeed")
	}
	limiter.Enqueue(queued)
	if keys := release(true); len(keys) != 1 {
		t.Fatalf("release wake keys = %v, want one key", keys)
	}

	if newRelease, ok := limiter.TryAcquire("", "openai-chat", provider); ok {
		newRelease()
		t.Fatal("new request should not acquire after release created a queued reservation")
	}
	wokenRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, queued.RequestID)
	if !ok || wokenRelease == nil {
		t.Fatal("woken request should acquire reserved slot after release")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-a"), queued.RequestID)
	wokenRelease(false)
}

func TestProviderConcurrencyLimiterGenericWakeReservesCandidateProviders(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerA := Provider{ID: 1, MaxConcurrency: 1}
	providerB := Provider{ID: 2, MaxConcurrency: 1}
	queued := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{providerA, providerB},
		ProviderIDs: []int64{1, 2},
	}

	limiter.Enqueue(queued)
	limiter.WakeQueue("", "openai-chat", "pool-a")

	if release, ok := limiter.TryAcquire("", "openai-chat", providerB); ok {
		release()
		t.Fatal("new request should not acquire provider reserved by generic wake candidates")
	}
	wokenRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", providerB, queued.RequestID)
	if !ok || wokenRelease == nil {
		t.Fatal("generic-woken request should acquire any candidate provider")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-a"), queued.RequestID)
	wokenRelease(false)
}

func TestProviderConcurrencyLimiterFlexibleProviderWakeUsesGenericReservation(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerA := Provider{ID: 1, MaxConcurrency: 1}
	providerB := Provider{ID: 2, MaxConcurrency: 1}
	queued := &ProviderQueueItem{
		RequestID:        1,
		Platform:         "openai-chat",
		PoolID:           "pool-a",
		Providers:        []Provider{providerA, providerB},
		ProviderIDs:      []int64{1, 2},
		FlexibleProvider: true,
	}

	limiter.Enqueue(queued)
	if keys := limiter.WakeQueueForProvider("", "openai-chat", providerA); len(keys) != 1 {
		t.Fatalf("wake keys = %v, want one key", keys)
	}
	if providerID, ok := limiter.ReservedProviderForRequest(queued.RequestID); ok {
		t.Fatalf("flexible wake should not bind a provider, got provider %d", providerID)
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", providerB); ok {
		release()
		t.Fatal("new request should not acquire another candidate provider while flexible request is woken")
	}
	wokenRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", providerB, queued.RequestID)
	if !ok || wokenRelease == nil {
		t.Fatal("flexible-woken request should acquire a fresh candidate provider")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-a"), queued.RequestID)
	wokenRelease(false)
}

func TestProviderConcurrencyLimiterRefreshWokenProvidersMovesGenericReservation(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerA := Provider{ID: 1, MaxConcurrency: 1}
	providerB := Provider{ID: 2, MaxConcurrency: 1}
	queued := &ProviderQueueItem{
		RequestID:        1,
		Platform:         "openai-chat",
		PoolID:           "pool-a",
		Providers:        []Provider{providerA},
		ProviderIDs:      []int64{1},
		FlexibleProvider: true,
	}

	limiter.Enqueue(queued)
	limiter.WakeQueue("", "openai-chat", "pool-a")
	releasedProviders, refreshed := limiter.RefreshWokenProvidersForRequest(queued.RequestID, []Provider{providerB})
	if !refreshed {
		t.Fatal("flexible generic reservation should refresh")
	}
	if len(releasedProviders) != 1 || releasedProviders[0].ID != providerA.ID {
		t.Fatalf("released providers = %v, want provider A", releasedProviders)
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", providerA); !ok || release == nil {
		t.Fatal("provider A should be available after refresh moves reservation to provider B")
	} else {
		release()
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", providerB); ok {
		release()
		t.Fatal("provider B should be reserved after refresh")
	}
	wokenRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", providerB, queued.RequestID)
	if !ok || wokenRelease == nil {
		t.Fatal("refreshed woken request should acquire provider B")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-a"), queued.RequestID)
	wokenRelease(false)
}

func TestProviderConcurrencyLimiterRefreshWokenProvidersAndReserveNextTransfersReleasedProviderAtomically(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerA := Provider{ID: 1, MaxConcurrency: 1}
	providerB := Provider{ID: 2, MaxConcurrency: 1}
	woken := &ProviderQueueItem{
		RequestID:        1,
		Platform:         "openai-chat",
		PoolID:           "pool-a",
		Providers:        []Provider{providerA, providerB},
		ProviderIDs:      []int64{1, 2},
		FlexibleProvider: true,
	}
	waitingA := &ProviderQueueItem{
		RequestID:   2,
		Platform:    "openai-chat",
		PoolID:      "pool-b",
		Providers:   []Provider{providerA},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(woken)
	limiter.WakeQueue("", "openai-chat", "pool-a")
	limiter.Enqueue(waitingA)
	keys, refreshed := limiter.RefreshWokenProvidersForRequestAndReserveNext(woken.RequestID, []Provider{providerB}, "", "openai-chat")
	if !refreshed {
		t.Fatal("flexible generic reservation should refresh")
	}
	if len(keys) != 1 || keys[0] != providerQueueKey("", "openai-chat", "pool-b") {
		t.Fatalf("refresh handoff keys = %v, want pool-b", keys)
	}
	select {
	case <-waitingA.Ready:
	default:
		t.Fatal("provider A queue should wake when provider A is released from generic reservation")
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", providerA); ok {
		release()
		t.Fatal("new request should not acquire provider A reserved during refresh handoff")
	}
	waitingRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", providerA, waitingA.RequestID)
	if !ok || waitingRelease == nil {
		t.Fatal("provider A handoff target should acquire reserved provider")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-b"), waitingA.RequestID)
	waitingRelease(false)
}

func TestProviderConcurrencyLimiterCompleteGenericWakeReturnsReservedProviders(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerA := Provider{ID: 1, MaxConcurrency: 1}
	providerB := Provider{ID: 2, MaxConcurrency: 1}
	queued := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{providerA, providerB},
		ProviderIDs: []int64{1, 2},
	}

	limiter.Enqueue(queued)
	limiter.WakeQueue("", "openai-chat", "pool-a")
	providerID, providers, completed := limiter.CompleteWakeWithInfo(providerQueueKey("", "openai-chat", "pool-a"), queued.RequestID)
	if !completed || providerID != 0 {
		t.Fatalf("CompleteWakeWithInfo = (%d,%v), want generic completed", providerID, completed)
	}
	if len(providers) != 2 || !providerIDInProviders(providers, providerA.ID) || !providerIDInProviders(providers, providerB.ID) {
		t.Fatalf("completed providers = %v, want A and B", providers)
	}
}

func TestProviderConcurrencyLimiterCompleteWakeFallsBackToRequestIDWhenQueueKeyChanges(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}
	queued := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(queued)
	limiter.WakeQueueForProvider("", "openai-chat", provider)

	wrongQueueKey := providerQueueKey("", "openai-chat", "pool-b")
	providerID, providers, completed := limiter.CompleteWakeWithInfo(wrongQueueKey, queued.RequestID)
	if !completed || providerID != provider.ID || len(providers) != 0 {
		t.Fatalf("CompleteWakeWithInfo with changed key = (%d,%v,%v), want provider-specific completion", providerID, providers, completed)
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", provider); !ok || release == nil {
		t.Fatal("provider slot should be available after completing wake by request ID fallback")
	} else {
		release()
	}
}

func TestProviderConcurrencyLimiterReenqueueClearsOldWokenReservationAcrossQueueKeys(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}
	moved := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}
	ahead := &ProviderQueueItem{
		RequestID:   2,
		Platform:    "openai-chat",
		PoolID:      "pool-b",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(moved)
	limiter.WakeQueueForProvider("", "openai-chat", provider)
	limiter.Enqueue(ahead)

	moved.PoolID = "pool-b"
	position, keys := limiter.EnqueueWithHandoff(moved)
	if position != 2 {
		t.Fatalf("moved request position = %d, want 2", position)
	}
	if !stringInSlice(keys, providerQueueKey("", "openai-chat", "pool-a")) || !stringInSlice(keys, providerQueueKey("", "openai-chat", "pool-b")) {
		t.Fatalf("handoff keys = %v, want old pool-a and new pool-b", keys)
	}
	select {
	case <-ahead.Ready:
	default:
		t.Fatal("pool-b head should wake from old pool-a reservation handoff")
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", provider); ok {
		release()
		t.Fatal("new request should not acquire provider reserved for pool-b head")
	}
	aheadRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, ahead.RequestID)
	if !ok || aheadRelease == nil {
		t.Fatal("pool-b head should acquire handed-off provider reservation")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-b"), ahead.RequestID)
	aheadRelease(false)
	if release, ok := limiter.TryAcquire("", "openai-chat", provider); !ok || release == nil {
		t.Fatal("old pool-a reservation should not remain after pool-b head completes")
	} else {
		release()
	}
}

func TestProviderConcurrencyLimiterReenqueueClearsOldGenericWokenReservationAcrossQueueKeys(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerA := Provider{ID: 1, MaxConcurrency: 1}
	providerB := Provider{ID: 2, MaxConcurrency: 1}
	moved := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{providerA, providerB},
		ProviderIDs: []int64{1, 2},
	}
	waitingA := &ProviderQueueItem{
		RequestID:   2,
		Platform:    "openai-chat",
		PoolID:      "pool-c",
		Providers:   []Provider{providerA},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(moved)
	limiter.WakeQueue("", "openai-chat", "pool-a")
	limiter.Enqueue(waitingA)

	moved.PoolID = "pool-b"
	moved.Providers = []Provider{providerB}
	moved.ProviderIDs = []int64{2}
	position, keys := limiter.EnqueueWithHandoff(moved)
	if position != 1 {
		t.Fatalf("moved request position = %d, want 1", position)
	}
	if !stringInSlice(keys, providerQueueKey("", "openai-chat", "pool-a")) ||
		!stringInSlice(keys, providerQueueKey("", "openai-chat", "pool-b")) ||
		!stringInSlice(keys, providerQueueKey("", "openai-chat", "pool-c")) {
		t.Fatalf("generic handoff keys = %v, want pool-a, pool-b and pool-c", keys)
	}
	select {
	case <-waitingA.Ready:
	default:
		t.Fatal("provider A queue should wake from old generic reservation handoff")
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", providerA); ok {
		release()
		t.Fatal("new request should not acquire provider A reserved for generic handoff target")
	}
	waitingRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", providerA, waitingA.RequestID)
	if !ok || waitingRelease == nil {
		t.Fatal("provider A generic handoff target should acquire reserved provider")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-c"), waitingA.RequestID)
	waitingRelease(false)
	if release, ok := limiter.TryAcquire("", "openai-chat", providerA); !ok || release == nil {
		t.Fatal("old generic provider A reservation should not remain")
	} else {
		release()
	}
}

func TestProviderConcurrencyLimiterCompleteWakeAndReserveNextTransfersReservationAtomically(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}
	woken := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}
	next := &ProviderQueueItem{
		RequestID:   2,
		Platform:    "openai-chat",
		PoolID:      "pool-b",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(woken)
	limiter.WakeQueueForProvider("", "openai-chat", provider)
	limiter.Enqueue(next)
	wrongQueueKey := providerQueueKey("", "openai-chat", "pool-c")
	keys := limiter.CompleteWakeAndReserveNext(wrongQueueKey, woken.RequestID, "", "openai-chat")
	if len(keys) != 1 || keys[0] != providerQueueKey("", "openai-chat", "pool-b") {
		t.Fatalf("handoff keys = %v, want pool-b", keys)
	}
	select {
	case <-next.Ready:
	default:
		t.Fatal("next queued request should wake during atomic handoff")
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", provider); ok {
		release()
		t.Fatal("new request should not acquire provider reserved for handoff")
	}
	wokenRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, next.RequestID)
	if !ok || wokenRelease == nil {
		t.Fatal("handoff target should acquire reserved provider")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-b"), next.RequestID)
	wokenRelease(false)
}

func TestProviderConcurrencyLimiterRemoveWokenAndReserveNextTransfersReservationAtomically(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}
	woken := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}
	next := &ProviderQueueItem{
		RequestID:   2,
		Platform:    "openai-chat",
		PoolID:      "pool-b",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(woken)
	limiter.WakeQueueForProvider("", "openai-chat", provider)
	limiter.Enqueue(next)
	removed, keys := limiter.RemoveQueuedAndReserveNext(woken.RequestID, "", "openai-chat")
	if !removed {
		t.Fatal("woken request should be removed")
	}
	if len(keys) != 2 || !stringInSlice(keys, providerQueueKey("", "openai-chat", "pool-a")) || !stringInSlice(keys, providerQueueKey("", "openai-chat", "pool-b")) {
		t.Fatalf("remove handoff keys = %v, want pool-a and pool-b", keys)
	}
	select {
	case <-next.Ready:
	default:
		t.Fatal("next queued request should wake during remove handoff")
	}
	if release, ok := limiter.TryAcquire("", "openai-chat", provider); ok {
		release()
		t.Fatal("new request should not acquire provider reserved for remove handoff")
	}
	wokenRelease, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, next.RequestID)
	if !ok || wokenRelease == nil {
		t.Fatal("remove handoff target should acquire reserved provider")
	}
	limiter.CompleteWake(providerQueueKey("", "openai-chat", "pool-b"), next.RequestID)
	wokenRelease(false)
}

func TestProviderRelayCompleteGenericWakeAfterAcquireFansOutReleasedProviders(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	providerA := Provider{ID: 1, Name: "provider-a", MaxConcurrency: 1}
	providerB := Provider{ID: 2, Name: "provider-b", MaxConcurrency: 1}
	woken := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{providerA, providerB},
		ProviderIDs: []int64{1, 2},
	}
	waitingA := &ProviderQueueItem{
		RequestID:   2,
		Platform:    "openai-chat",
		PoolID:      "pool-b",
		Providers:   []Provider{providerA},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(woken)
	limiter.WakeQueue("", "openai-chat", "pool-a")
	limiter.Enqueue(waitingA)
	release, ok := limiter.TryAcquireForRequest("", "openai-chat", providerB, woken.RequestID)
	if !ok || release == nil {
		t.Fatal("woken request should acquire provider B")
	}

	relay := &ProviderRelayService{concurrencyLimiter: limiter}
	relay.completeQueueWakeAfterAcquire("", "openai-chat", providerQueueKey("", "openai-chat", "pool-a"), woken.RequestID, providerB)
	select {
	case <-waitingA.Ready:
	default:
		t.Fatal("provider A queue should wake after generic reservation is completed by provider B acquire")
	}
	if newRelease, ok := limiter.TryAcquire("", "openai-chat", providerA); ok {
		newRelease()
		t.Fatal("new request should not acquire provider A reserved by the fan-out handoff")
	}
	release(false)
}

func TestProviderRelayCompleteWakeAfterAcquireClearsReservationWhenPoolBindingChanges(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, Name: "provider-a", MaxConcurrency: 1}
	woken := &ProviderQueueItem{
		RequestID:   1,
		Platform:    "openai-chat",
		PoolID:      "pool-a",
		Providers:   []Provider{provider},
		ProviderIDs: []int64{1},
	}

	limiter.Enqueue(woken)
	limiter.WakeQueueForProvider("", "openai-chat", provider)
	release, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, woken.RequestID)
	if !ok || release == nil {
		t.Fatal("woken request should acquire reserved provider")
	}

	relay := &ProviderRelayService{concurrencyLimiter: limiter}
	relay.completeQueueWakeAfterAcquire("", "openai-chat", providerQueueKey("", "openai-chat", "pool-b"), woken.RequestID, provider)
	release(false)

	if nextRelease, ok := limiter.TryAcquire("", "openai-chat", provider); !ok || nextRelease == nil {
		t.Fatal("reservation should be cleared even when completion used a changed pool queue key")
	} else {
		nextRelease()
	}
}

func TestProviderConcurrencyLimiterWakeAfterLateEnqueueUsesAvailableCapacity(t *testing.T) {
	limiter := NewProviderConcurrencyLimiter()
	provider := Provider{ID: 1, MaxConcurrency: 1}

	release, ok := limiter.TryAcquireForRequest("", "openai-chat", provider, 10)
	if !ok || release == nil {
		t.Fatal("initial acquire should succeed")
	}
	if _, ok := limiter.TryAcquire("", "openai-chat", provider); ok {
		t.Fatal("provider should be full before release")
	}
	if keys := release(true); len(keys) != 0 {
		t.Fatalf("release before enqueue should not wake queues, got %v", keys)
	}

	queued := &ProviderQueueItem{RequestID: 1, Platform: "openai-chat", PoolID: "pool-a", Providers: []Provider{provider}, ProviderIDs: []int64{1}}
	limiter.Enqueue(queued)
	if keys := limiter.WakeQueueForProvider("", "openai-chat", provider); len(keys) != 1 {
		t.Fatalf("late enqueue wake keys = %v, want one key", keys)
	}
	select {
	case <-queued.Ready:
	default:
		t.Fatal("late-enqueued request should wake when provider capacity is available")
	}
}

func stringInSlice(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
