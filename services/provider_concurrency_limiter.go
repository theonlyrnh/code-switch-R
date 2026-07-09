package services

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type ProviderConcurrencyLimiter struct {
	mu             sync.Mutex
	inFlight       map[string]int
	queues         map[string][]*ProviderQueueItem
	woken          map[string]int64
	wokenProvider  map[string]int64
	wokenProviders map[string][]Provider
}

type ProviderQueueItem struct {
	RequestID        int64
	UserID           string
	Platform         string
	PoolID           string
	Model            string
	Providers        []Provider
	ProviderIDs      []int64
	FlexibleProvider bool
	EnqueuedAt       time.Time
	Ready            chan struct{}
}

func NewProviderConcurrencyLimiter() *ProviderConcurrencyLimiter {
	return &ProviderConcurrencyLimiter{
		inFlight:       make(map[string]int),
		queues:         make(map[string][]*ProviderQueueItem),
		woken:          make(map[string]int64),
		wokenProvider:  make(map[string]int64),
		wokenProviders: make(map[string][]Provider),
	}
}

func providerConcurrencyKey(userID, platform string, providerID int64) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Sprintf("%s:%d", platform, providerID)
	}
	return fmt.Sprintf("%s:%s:%d", userID, platform, providerID)
}

func providerQueueKey(userID, platform, poolID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return platform + ":" + poolID
	}
	return userID + ":" + platform + ":" + poolID
}

func (l *ProviderConcurrencyLimiter) TryAcquire(userID, platform string, provider Provider) (func(), bool) {
	releaseWithWake, ok := l.TryAcquireForRequest(userID, platform, provider, 0)
	if !ok {
		return nil, false
	}
	return func() {
		releaseWithWake(false)
	}, true
}

func (l *ProviderConcurrencyLimiter) TryAcquireForRequest(userID, platform string, provider Provider, requestID int64) (func(bool) []string, bool) {
	if l == nil {
		return func(bool) []string { return nil }, true
	}
	limit := provider.NormalizedMaxConcurrency()
	key := providerConcurrencyKey(userID, platform, provider.ID)

	l.mu.Lock()
	defer l.mu.Unlock()

	if reservedProviderID, ok := l.reservedProviderForRequestLocked(requestID); ok && reservedProviderID != provider.ID {
		return nil, false
	}
	if l.inFlight[key]+l.reservedForProviderLocked(userID, platform, provider.ID, requestID) >= limit {
		return nil, false
	}
	l.inFlight[key]++

	released := false
	return func(wake bool) []string {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return nil
		}
		released = true
		l.releaseProviderLocked(key)
		if wake {
			return l.wakeQueueForProviderLocked(userID, platform, provider)
		}
		return nil
	}, true
}

func (l *ProviderConcurrencyLimiter) releaseProviderLocked(key string) {
	current := l.inFlight[key]
	if current <= 1 {
		delete(l.inFlight, key)
		return
	}
	l.inFlight[key] = current - 1
}

func (l *ProviderConcurrencyLimiter) reservedForProviderLocked(userID, platform string, providerID int64, requestID int64) int {
	if l == nil || providerID == 0 {
		return 0
	}
	reserved := 0
	for queueKey, wokenRequestID := range l.woken {
		if requestID != 0 && wokenRequestID == requestID {
			continue
		}
		if reservedProviderID := l.wokenProvider[queueKey]; reservedProviderID != 0 {
			if reservedProviderID != providerID {
				continue
			}
		} else if !providerIDInProviders(l.wokenProviders[queueKey], providerID) {
			continue
		}
		if !providerQueueKeyMatchesUserPlatform(queueKey, userID, platform) {
			continue
		}
		reserved++
	}
	return reserved
}

func (l *ProviderConcurrencyLimiter) ReservedProviderForRequest(requestID int64) (int64, bool) {
	if l == nil || requestID == 0 {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reservedProviderForRequestLocked(requestID)
}

func (l *ProviderConcurrencyLimiter) reservedProviderForRequestLocked(requestID int64) (int64, bool) {
	for queueKey, wokenRequestID := range l.woken {
		if wokenRequestID == requestID {
			providerID := l.wokenProvider[queueKey]
			return providerID, providerID != 0
		}
	}
	return 0, false
}

func (l *ProviderConcurrencyLimiter) providerCapacityAvailableLocked(userID, platform string, provider Provider) bool {
	if l == nil || provider.ID == 0 {
		return false
	}
	key := providerConcurrencyKey(userID, platform, provider.ID)
	return l.inFlight[key]+l.reservedForProviderLocked(userID, platform, provider.ID, 0) < provider.NormalizedMaxConcurrency()
}

func (l *ProviderConcurrencyLimiter) Enqueue(item *ProviderQueueItem) int {
	position, _ := l.enqueue(item, false)
	return position
}

func (l *ProviderConcurrencyLimiter) EnqueueFront(item *ProviderQueueItem) int {
	position, _ := l.enqueue(item, true)
	return position
}

func (l *ProviderConcurrencyLimiter) EnqueueWithHandoff(item *ProviderQueueItem) (int, []string) {
	return l.enqueue(item, false)
}

func (l *ProviderConcurrencyLimiter) EnqueueFrontWithHandoff(item *ProviderQueueItem) (int, []string) {
	return l.enqueue(item, true)
}

func (l *ProviderConcurrencyLimiter) enqueue(item *ProviderQueueItem, front bool) (int, []string) {
	if l == nil || item == nil || item.RequestID == 0 {
		return 0, nil
	}
	item.EnqueuedAt = time.Now()
	item.Ready = make(chan struct{})
	key := providerQueueKey(item.UserID, item.Platform, item.PoolID)

	l.mu.Lock()
	defer l.mu.Unlock()

	var changedQueueKeys []string
	var handoffProviderIDs []int64
	var handoffProviders []Provider
	genericHandoff := false
	for queueKey, queue := range l.queues {
		next := removeQueueItem(queue, item.RequestID)
		if len(next) == len(queue) {
			continue
		}
		changedQueueKeys = appendStringUnique(changedQueueKeys, queueKey)
		if len(next) == 0 {
			delete(l.queues, queueKey)
		} else {
			l.queues[queueKey] = next
		}
	}
	for queueKey, wokenRequestID := range l.woken {
		if wokenRequestID != item.RequestID {
			continue
		}
		changedQueueKeys = appendStringUnique(changedQueueKeys, queueKey)
		providerID := l.wokenProvider[queueKey]
		if providerID != 0 {
			handoffProviderIDs = append(handoffProviderIDs, providerID)
		} else {
			genericHandoff = true
			handoffProviders = appendProvidersUnique(handoffProviders, l.wokenProviders[queueKey]...)
		}
		delete(l.woken, queueKey)
		delete(l.wokenProvider, queueKey)
		delete(l.wokenProviders, queueKey)
	}

	queue := l.queues[key]
	if front {
		queue = append([]*ProviderQueueItem{item}, queue...)
	} else {
		queue = append(queue, item)
	}
	l.queues[key] = queue
	changedQueueKeys = appendStringUnique(changedQueueKeys, key)
	position := queuePositionLocked(queue, item.RequestID)

	for _, providerID := range handoffProviderIDs {
		changedQueueKeys = appendStringUnique(changedQueueKeys, l.wakeQueueForReservedProviderLocked(item.UserID, item.Platform, providerID)...)
	}
	if genericHandoff {
		changedQueueKeys = appendStringUnique(changedQueueKeys, l.wakeQueuesForProvidersLocked(item.UserID, item.Platform, handoffProviders)...)
	}

	return position, changedQueueKeys
}

func (l *ProviderConcurrencyLimiter) RemoveQueued(requestID int64) bool {
	removed, _ := l.RemoveQueuedWithStatus(requestID)
	return removed
}

func (l *ProviderConcurrencyLimiter) RemoveQueuedWithStatus(requestID int64) (bool, bool) {
	removed, wasWoken, _, _, _ := l.RemoveQueuedWithWakeInfo(requestID)
	return removed, wasWoken
}

func (l *ProviderConcurrencyLimiter) RemoveQueuedWithWakeInfo(requestID int64) (bool, bool, int64, []Provider, bool) {
	if l == nil || requestID == 0 {
		return false, false, 0, nil, false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	removed := false
	wasWoken := false
	wokenProviderID := int64(0)
	var exposedProviders []Provider
	exposedHead := false
	for key, queue := range l.queues {
		removedHead := false
		matchedRequest := false
		next := queue[:0]
		for i, item := range queue {
			if item == nil {
				continue
			}
			if item.RequestID == requestID {
				matchedRequest = true
				removedHead = i == 0
				continue
			}
			next = append(next, item)
		}
		if len(next) != len(queue) {
			removed = true
			if len(next) == 0 {
				delete(l.queues, key)
			} else {
				l.queues[key] = next
			}
		}
		if matchedRequest && removedHead && l.woken[key] == 0 && len(next) > 0 {
			exposedHead = true
			exposedProviders = appendProvidersUnique(exposedProviders, providerQueueItemProviders(next[0])...)
		}
	}
	for key, id := range l.woken {
		if id == requestID {
			delete(l.woken, key)
			removed = true
			wasWoken = true
			wokenProviderID = l.wokenProvider[key]
			if wokenProviderID == 0 {
				exposedProviders = appendProvidersUnique(exposedProviders, l.wokenProviders[key]...)
			}
			delete(l.wokenProvider, key)
			delete(l.wokenProviders, key)
		}
	}
	return removed, wasWoken, wokenProviderID, exposedProviders, exposedHead
}

func (l *ProviderConcurrencyLimiter) RemoveQueuedAndReserveNext(requestID int64, userID, platform string) (bool, []string) {
	if l == nil || requestID == 0 {
		return false, nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	removed := false
	var changedQueueKeys []string
	var exposedProviders []Provider
	exposedHead := false
	var handoffProviders []Provider
	var handoffProviderIDs []int64
	genericHandoff := false
	for key, queue := range l.queues {
		removedHead := false
		matchedRequest := false
		next := queue[:0]
		for i, item := range queue {
			if item == nil {
				continue
			}
			if item.RequestID == requestID {
				matchedRequest = true
				removedHead = i == 0
				continue
			}
			next = append(next, item)
		}
		if len(next) != len(queue) {
			removed = true
			changedQueueKeys = appendStringUnique(changedQueueKeys, key)
			if len(next) == 0 {
				delete(l.queues, key)
			} else {
				l.queues[key] = next
			}
		}
		if matchedRequest && removedHead && l.woken[key] == 0 && len(next) > 0 {
			exposedHead = true
			exposedProviders = appendProvidersUnique(exposedProviders, providerQueueItemProviders(next[0])...)
		}
	}
	for key, id := range l.woken {
		if id != requestID {
			continue
		}
		removed = true
		changedQueueKeys = appendStringUnique(changedQueueKeys, key)
		providerID := l.wokenProvider[key]
		if providerID != 0 {
			handoffProviderIDs = append(handoffProviderIDs, providerID)
		} else {
			genericHandoff = true
			handoffProviders = appendProvidersUnique(handoffProviders, l.wokenProviders[key]...)
		}
		delete(l.woken, key)
		delete(l.wokenProvider, key)
		delete(l.wokenProviders, key)
	}
	for _, providerID := range handoffProviderIDs {
		changedQueueKeys = appendStringUnique(changedQueueKeys, l.wakeQueueForReservedProviderLocked(userID, platform, providerID)...)
	}
	if genericHandoff {
		changedQueueKeys = appendStringUnique(changedQueueKeys, l.wakeQueuesForProvidersLocked(userID, platform, handoffProviders)...)
	}
	if exposedHead {
		changedQueueKeys = appendStringUnique(changedQueueKeys, l.wakeQueuesForProvidersLocked(userID, platform, exposedProviders)...)
	}
	return removed, changedQueueKeys
}

func (l *ProviderConcurrencyLimiter) QueuePosition(requestID int64) (int, bool) {
	if l == nil || requestID == 0 {
		return 0, false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for _, queue := range l.queues {
		if position := queuePositionLocked(queue, requestID); position > 0 {
			return position, true
		}
	}
	return 0, false
}

func (l *ProviderConcurrencyLimiter) QueuePositions(queueKey string) map[int64]int {
	if l == nil || strings.TrimSpace(queueKey) == "" {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	queue := l.queues[queueKey]
	positions := make(map[int64]int, len(queue))
	for i, item := range queue {
		if item != nil && item.RequestID != 0 {
			positions[item.RequestID] = i + 1
		}
	}
	return positions
}

func (l *ProviderConcurrencyLimiter) WakeQueue(userID, platform, poolID string) {
	if l == nil {
		return
	}
	key := providerQueueKey(userID, platform, poolID)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.wakeQueueLocked(key, 0)
}

func (l *ProviderConcurrencyLimiter) WakeQueuesForPlatform(userID, platform string) []string {
	if l == nil || strings.TrimSpace(platform) == "" {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.wakeQueuesForPlatformLocked(userID, platform)
}

func (l *ProviderConcurrencyLimiter) WakeQueueForProvider(userID, platform string, provider Provider) []string {
	if l == nil || strings.TrimSpace(platform) == "" || provider.ID == 0 {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.wakeQueueForProviderLocked(userID, platform, provider)
}

func (l *ProviderConcurrencyLimiter) WakeQueueForReservedProvider(userID, platform string, providerID int64) []string {
	if l == nil || strings.TrimSpace(platform) == "" || providerID == 0 {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.wakeQueueForReservedProviderLocked(userID, platform, providerID)
}

func (l *ProviderConcurrencyLimiter) wakeQueueForProviderLocked(userID, platform string, provider Provider) []string {
	if !l.providerCapacityAvailableLocked(userID, platform, provider) {
		return nil
	}
	return l.wakeQueueForReservedProviderLocked(userID, platform, provider.ID)
}

func (l *ProviderConcurrencyLimiter) wakeQueueForReservedProviderLocked(userID, platform string, providerID int64) []string {
	var selectedKey string
	var selectedAt time.Time
	for key, queue := range l.queues {
		if !providerQueueKeyMatchesUserPlatform(key, userID, platform) {
			continue
		}
		if l.woken[key] != 0 || len(queue) == 0 {
			continue
		}
		item := queue[0]
		if !providerQueueItemCanUseProvider(item, providerID) {
			continue
		}
		if selectedKey == "" || item.EnqueuedAt.Before(selectedAt) {
			selectedKey = key
			selectedAt = item.EnqueuedAt
		}
	}
	if selectedKey == "" {
		return nil
	}
	reservationProviderID := providerID
	if queue := l.queues[selectedKey]; len(queue) > 0 && queue[0] != nil && queue[0].FlexibleProvider {
		reservationProviderID = 0
	}
	if l.wakeQueueLocked(selectedKey, reservationProviderID) {
		return []string{selectedKey}
	}
	return nil
}

func (l *ProviderConcurrencyLimiter) wakeQueueLocked(key string, providerID int64) bool {
	if l.woken[key] != 0 {
		return false
	}
	queue := l.queues[key]
	if len(queue) == 0 {
		return false
	}
	item := queue[0]
	queue = queue[1:]
	if len(queue) == 0 {
		delete(l.queues, key)
	} else {
		l.queues[key] = queue
	}
	if item == nil || item.RequestID == 0 || item.Ready == nil {
		return false
	}
	l.woken[key] = item.RequestID
	if providerID != 0 {
		l.wokenProvider[key] = providerID
		delete(l.wokenProviders, key)
	} else {
		delete(l.wokenProvider, key)
		providers := providerQueueItemProviders(item)
		if len(providers) == 0 {
			delete(l.wokenProviders, key)
		} else {
			l.wokenProviders[key] = providers
		}
	}
	close(item.Ready)
	return true
}

func (l *ProviderConcurrencyLimiter) CompleteWake(queueKey string, requestID int64) (int64, bool) {
	providerID, _, completed := l.CompleteWakeWithInfo(queueKey, requestID)
	return providerID, completed
}

func (l *ProviderConcurrencyLimiter) CompleteWakeWithInfo(queueKey string, requestID int64) (int64, []Provider, bool) {
	if l == nil || requestID == 0 {
		return 0, nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key, ok := l.wokenQueueKeyLocked(queueKey, requestID)
	if !ok {
		return 0, nil, false
	}
	return l.completeWakeLocked(key)
}

func (l *ProviderConcurrencyLimiter) completeWakeLocked(queueKey string) (int64, []Provider, bool) {
	providerID := l.wokenProvider[queueKey]
	providers := l.wokenProviders[queueKey]
	delete(l.woken, queueKey)
	delete(l.wokenProvider, queueKey)
	delete(l.wokenProviders, queueKey)
	return providerID, providers, true
}

func (l *ProviderConcurrencyLimiter) wokenQueueKeyLocked(queueKey string, requestID int64) (string, bool) {
	if requestID == 0 {
		return "", false
	}
	if strings.TrimSpace(queueKey) != "" && l.woken[queueKey] == requestID {
		return queueKey, true
	}
	for key, wokenRequestID := range l.woken {
		if wokenRequestID == requestID {
			return key, true
		}
	}
	return "", false
}

func (l *ProviderConcurrencyLimiter) CompleteWakeAndReserveNext(queueKey string, requestID int64, userID, platform string) []string {
	if l == nil || requestID == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	key, ok := l.wokenQueueKeyLocked(queueKey, requestID)
	if !ok {
		return nil
	}
	providerID, providers, _ := l.completeWakeLocked(key)
	if providerID != 0 {
		return l.wakeQueueForReservedProviderLocked(userID, platform, providerID)
	}
	return l.wakeQueuesForProvidersLocked(userID, platform, providers)
}

func (l *ProviderConcurrencyLimiter) CompleteWakeAfterAcquire(queueKey string, requestID int64, userID, platform string, provider Provider) []string {
	if l == nil || requestID == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	key, ok := l.wokenQueueKeyLocked(queueKey, requestID)
	if !ok {
		return nil
	}
	providerID, providers, _ := l.completeWakeLocked(key)
	if providerID != 0 {
		if provider.ID == 0 {
			provider.ID = providerID
		}
		return l.wakeQueueForProviderLocked(userID, platform, provider)
	}
	if provider.ID != 0 {
		providers = appendProvidersUnique(providers, provider)
	}
	return l.wakeQueuesForProvidersLocked(userID, platform, providers)
}

func (l *ProviderConcurrencyLimiter) wakeQueuesForProvidersLocked(userID, platform string, providers []Provider) []string {
	if len(providers) == 0 {
		return l.wakeQueuesForPlatformLocked(userID, platform)
	}
	var keys []string
	for _, provider := range providers {
		for _, key := range l.wakeQueueForProviderLocked(userID, platform, provider) {
			keys = appendStringUnique(keys, key)
		}
	}
	return keys
}

func (l *ProviderConcurrencyLimiter) wakeQueuesForPlatformLocked(userID, platform string) []string {
	keys := make([]string, 0)
	for key := range l.queues {
		if !providerQueueKeyMatchesUserPlatform(key, userID, platform) {
			continue
		}
		if l.wakeQueueLocked(key, 0) {
			keys = append(keys, key)
		}
	}
	return keys
}

func (l *ProviderConcurrencyLimiter) RefreshWokenProvidersForRequest(requestID int64, providers []Provider) ([]Provider, bool) {
	if l == nil || requestID == 0 || len(providers) == 0 {
		return nil, false
	}
	nextProviders := uniqueProviders(providers)
	if len(nextProviders) == 0 {
		return nil, false
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for queueKey, wokenRequestID := range l.woken {
		if wokenRequestID != requestID || l.wokenProvider[queueKey] != 0 {
			continue
		}
		previousProviders := l.wokenProviders[queueKey]
		l.wokenProviders[queueKey] = nextProviders
		return providersWithoutIDs(previousProviders, nextProviders), true
	}
	return nil, false
}

func (l *ProviderConcurrencyLimiter) RefreshWokenProvidersForRequestAndReserveNext(requestID int64, providers []Provider, userID, platform string) ([]string, bool) {
	if l == nil || requestID == 0 || len(providers) == 0 {
		return nil, false
	}
	nextProviders := uniqueProviders(providers)
	if len(nextProviders) == 0 {
		return nil, false
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for queueKey, wokenRequestID := range l.woken {
		if wokenRequestID != requestID || l.wokenProvider[queueKey] != 0 {
			continue
		}
		previousProviders := l.wokenProviders[queueKey]
		l.wokenProviders[queueKey] = nextProviders
		releasedProviders := providersWithoutIDs(previousProviders, nextProviders)
		if len(releasedProviders) == 0 {
			return nil, true
		}
		return l.wakeQueuesForProvidersLocked(userID, platform, releasedProviders), true
	}
	return nil, false
}

func (l *ProviderConcurrencyLimiter) HasQueueAhead(queueKey string, requestID int64) bool {
	if l == nil || strings.TrimSpace(queueKey) == "" {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if woken := l.woken[queueKey]; woken != 0 {
		return woken != requestID
	}
	queue := l.queues[queueKey]
	if len(queue) == 0 {
		return false
	}
	if queue[0] == nil {
		return false
	}
	return queue[0].RequestID != requestID
}

func removeQueueItem(queue []*ProviderQueueItem, requestID int64) []*ProviderQueueItem {
	if len(queue) == 0 {
		return queue
	}
	next := queue[:0]
	for _, item := range queue {
		if item == nil || item.RequestID == requestID {
			continue
		}
		next = append(next, item)
	}
	return next
}

func queuePositionLocked(queue []*ProviderQueueItem, requestID int64) int {
	for i, item := range queue {
		if item != nil && item.RequestID == requestID {
			return i + 1
		}
	}
	return 0
}

func providerQueueKeyMatchesUserPlatform(key, userID, platform string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return strings.HasPrefix(key, platform+":")
	}
	return strings.HasPrefix(key, userID+":"+platform+":")
}

func providerQueueItemCanUseProvider(item *ProviderQueueItem, providerID int64) bool {
	if item == nil || providerID == 0 {
		return true
	}
	if len(item.Providers) > 0 {
		for _, provider := range item.Providers {
			if provider.ID == providerID {
				return true
			}
		}
		return false
	}
	if len(item.ProviderIDs) == 0 {
		return true
	}
	for _, id := range item.ProviderIDs {
		if id == providerID {
			return true
		}
	}
	return false
}

func providerQueueItemProviders(item *ProviderQueueItem) []Provider {
	if item == nil {
		return nil
	}
	if len(item.Providers) > 0 {
		return append([]Provider(nil), item.Providers...)
	}
	providers := make([]Provider, 0, len(item.ProviderIDs))
	for _, id := range item.ProviderIDs {
		if id != 0 {
			providers = append(providers, Provider{ID: id, MaxConcurrency: 1})
		}
	}
	return providers
}

func providerIDInProviders(providers []Provider, providerID int64) bool {
	if providerID == 0 {
		return false
	}
	for _, provider := range providers {
		if provider.ID == providerID {
			return true
		}
	}
	return false
}

func uniqueProviders(providers []Provider) []Provider {
	return appendProvidersUnique(nil, providers...)
}

func providersWithoutIDs(providers []Provider, excluded []Provider) []Provider {
	var result []Provider
	for _, provider := range providers {
		if provider.ID == 0 || providerIDInProviders(excluded, provider.ID) {
			continue
		}
		result = appendProvidersUnique(result, provider)
	}
	return result
}

func appendStringUnique(dst []string, values ...string) []string {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		exists := false
		for _, existing := range dst {
			if existing == value {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, value)
		}
	}
	return dst
}

func appendProvidersUnique(dst []Provider, providers ...Provider) []Provider {
	for _, provider := range providers {
		if provider.ID == 0 {
			continue
		}
		exists := false
		for _, existing := range dst {
			if existing.ID == provider.ID {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, provider)
		}
	}
	return dst
}
