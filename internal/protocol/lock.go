package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MaesterZ/git-remote-gs/internal/store"
)

// lockInfo names a lock's holder, for messages only; git-remote-s3 writes empty locks. Timestamp is Unix seconds.
type lockInfo struct {
	Timestamp float64 `json:"timestamp"`
	Host      string  `json:"host"`
	PID       int     `json:"pid"`
}

// errLockLost cancels a push whose lock another client took over, so its upload aborts before the object is committed.
var errLockLost = errors.New("lock lost")

// heldLock is a lock this helper holds; refreshes give it a new generation.
type heldLock struct {
	mu  sync.Mutex
	obj store.Object
}

func (l *heldLock) get() store.Object {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.obj
}

func (l *heldLock) set(obj store.Object) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.obj = obj
}

// withLock runs fn holding the ref's lock, refreshed until fn returns, and always releases it afterwards.
func (h *Helper) withLock(ctx context.Context, ref string, locked func(ctx context.Context) error) error {
	obj, err := h.acquireLock(ctx, ref)
	if err != nil {
		return err
	}

	lock := &heldLock{obj: obj}

	lockCtx, lost := context.WithCancelCause(ctx)
	defer lost(nil)

	stop, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		h.refreshLock(ctx, lock, stop, lost)
	}()

	err = locked(lockCtx)

	close(stop)
	<-done // a refresh in flight completes, so the release below uses the lock's latest generation

	if _, rejected := errors.AsType[*rejectError](err); err != nil && !rejected &&
		errors.Is(context.Cause(lockCtx), errLockLost) {
		err = reject("lost the lock %s to another push: this push's lock refreshes failed for over %s "+
			"(GIT_REMOTE_GS_LOCK_TTL); nothing was pushed, retry", h.cfg.Location.URL(obj.Name), seconds(h.cfg.LockTTL))
	}

	h.releaseLock(ctx, lock.get())

	return err
}

// refreshLock rewrites the lock with a new timestamp every LockRefresh, so a long upload doesn't look stale to others.
func (h *Helper) refreshLock(ctx context.Context, lock *heldLock, stop <-chan struct{}, lost context.CancelCauseFunc) {
	ticker := time.NewTicker(h.cfg.LockRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Not cancelled with the push: an interrupted rewrite would leave a generation the release doesn't know.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		cur := lock.get()
		obj, err := h.writeLock(writeCtx, cur.Name, cur.Generation)

		cancel()

		switch {
		case err == nil:
			lock.set(obj)
			h.cfg.Log.Logf("refreshed lock %s (generation %d)", h.cfg.Location.URL(obj.Name), obj.Generation)
		case errors.Is(err, store.ErrPrecondition), errors.Is(err, store.ErrNotFound):
			h.cfg.Log.Logf("lock %s was taken over: aborting the push", h.cfg.Location.URL(cur.Name))
			lost(errLockLost)

			return
		default:
			h.cfg.Log.Logf("could not refresh lock %s, retrying: %v", h.cfg.Location.URL(cur.Name), err)
		}
	}
}

func (h *Helper) acquireLock(ctx context.Context, ref string) (store.Object, error) {
	name := h.cfg.Location.Key(ref, lockObject)

	lock, err := h.writeLock(ctx, name, 0)
	if !errors.Is(err, store.ErrPrecondition) {
		return lock, err
	}

	held, err := h.statLock(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return h.retryLock(ctx, name, "") // released since our attempt
	}

	if err != nil {
		return store.Object{}, err
	}

	data, err := h.readSmall(ctx, name) // the holder's name, for messages only
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Object{}, err
	}

	holder, age := h.lockAge(data, held)
	if age <= h.cfg.LockTTL {
		return store.Object{}, h.lockBusy(name, holder)
	}

	h.cfg.Log.Logf("taking over stale lock %s (%s)", h.cfg.Location.URL(name), holder)

	err = h.cfg.Store.DeleteIfGeneration(ctx, name, held.Generation)
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrPrecondition) {
		return store.Object{}, err
	}

	return h.retryLock(ctx, name, holder)
}

// writeLock creates the lock (generation 0) or rewrites the one we hold, stamped with the current time.
func (h *Helper) writeLock(ctx context.Context, name string, generation int64) (store.Object, error) {
	body, err := json.Marshal(lockInfo{
		Timestamp: float64(h.cfg.Now().Unix()),
		Host:      h.cfg.Host,
		PID:       h.cfg.PID,
	})
	if err != nil {
		return store.Object{}, fmt.Errorf("encode lock: %w", err)
	}

	if generation != 0 {
		return h.cfg.Store.ReplaceIfGeneration(ctx, name, generation, bytes.NewReader(body), "application/json")
	}

	lock, err := h.cfg.Store.WriteIfAbsent(ctx, name, bytes.NewReader(body), "application/json")
	if err == nil {
		h.cfg.Log.Logf("locked %s (generation %d)", h.cfg.Location.URL(name), lock.Generation)
	}

	return lock, err
}

// retryLock makes the single retry allowed after a stale or vanished lock.
func (h *Helper) retryLock(ctx context.Context, name, holder string) (store.Object, error) {
	lock, err := h.writeLock(ctx, name, 0)
	if errors.Is(err, store.ErrPrecondition) {
		return store.Object{}, h.lockBusy(name, holder)
	}

	return lock, err
}

// statLock returns the lock's generation and server-side creation time, from a listing as git-remote-s3 uses HeadObject.
func (h *Helper) statLock(ctx context.Context, name string) (store.Object, error) {
	objs, err := h.cfg.Store.List(ctx, name)
	if err != nil {
		return store.Object{}, err
	}

	for _, obj := range objs {
		if obj.Name == name {
			return obj, nil
		}
	}

	return store.Object{}, fmt.Errorf("lock %s: %w", h.cfg.Location.URL(name), store.ErrNotFound)
}

// lockAge describes the lock holder, and how old the lock is from its object's time, as git-remote-s3 does.
func (h *Helper) lockAge(data []byte, obj store.Object) (string, time.Duration) {
	age := h.cfg.Now().Sub(obj.Created) // each refresh is a new generation, with its own creation time

	var info lockInfo

	err := json.Unmarshal(data, &info)
	if err != nil || info.Host == "" {
		return "held by an unknown client for " + seconds(age), age
	}

	return fmt.Sprintf("held by %s pid %d for %s", info.Host, info.PID, seconds(age)), age
}

func (h *Helper) lockBusy(name, holder string) error {
	url := h.cfg.Location.URL(name)

	if holder == "" {
		holder = "just taken by another push"
	}

	return reject("could not lock %s (%s): another push may be running. Retry, or after %s (GIT_REMOTE_GS_LOCK_TTL) "+
		"it is taken over; if no push is running, remove it with gcloud storage rm %s",
		url, holder, seconds(h.cfg.LockTTL), shellQuote(url))
}

// releaseLock deletes the lock only if it is still the generation we created; it never removes another client's lock.
func (h *Helper) releaseLock(ctx context.Context, lock store.Object) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	err := h.cfg.Store.DeleteIfGeneration(ctx, lock.Name, lock.Generation)

	switch {
	case err == nil:
		h.cfg.Log.Logf("unlocked %s", h.cfg.Location.URL(lock.Name))
	case errors.Is(err, store.ErrNotFound):
		h.cfg.Log.Logf("lock %s was already gone", h.cfg.Location.URL(lock.Name))
	case errors.Is(err, store.ErrPrecondition):
		h.cfg.Log.Warnf("lock %s was taken over while pushing: the push outlived GIT_REMOTE_GS_LOCK_TTL (%s)",
			h.cfg.Location.URL(lock.Name), seconds(h.cfg.LockTTL))
	default:
		h.cfg.Log.Warnf("could not release lock %s: %v; remove it with gcloud storage rm %s",
			h.cfg.Location.URL(lock.Name), err, shellQuote(h.cfg.Location.URL(lock.Name)))
	}
}

// seconds formats a duration the way GIT_REMOTE_GS_LOCK_TTL is set, e.g. "60s" rather than "1m0s".
func seconds(d time.Duration) string {
	return fmt.Sprintf("%ds", int64(d/time.Second))
}
