//go:build darwin && cgo

package calendar

/*
#cgo CFLAGS: -fblocks
#cgo LDFLAGS: -framework Foundation -framework EventKit -framework AppKit
#include <stdlib.h>
#include "eventkit_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"
)

type nativeEventStore struct {
	mu        sync.Mutex
	store     *C.bsbctl_calendar_store
	changeFD  *os.File
	changes   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

type authorizationRequest interface {
	Poll() (accessStatus, bool, error)
	Close()
}

type nativeAuthorizationRequest struct {
	request *C.bsbctl_calendar_authorization_request
}

func (r *nativeAuthorizationRequest) Poll() (accessStatus, bool, error) {
	var status C.int
	var nativeError *C.char
	completed := int(C.bsbctl_calendar_authorization_poll(r.request, &status, &nativeError))
	if completed < 0 {
		return accessUnknown, false, errors.New("poll EventKit authorization request")
	}
	if completed == 0 {
		return accessUnknown, false, nil
	}
	if nativeError != nil {
		return accessUnknown, true, consumeNativeCalendarError("request full Calendar access", &nativeError)
	}
	return accessStatusFromNative(int(status)), true, nil
}

func (r *nativeAuthorizationRequest) Close() {
	if r.request != nil {
		C.bsbctl_calendar_authorization_release(r.request)
		r.request = nil
	}
}

func waitForAuthorization(ctx context.Context, request authorizationRequest) (accessStatus, error) {
	defer request.Close()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, completed, err := request.Poll()
		if err != nil || completed {
			return status, err
		}
		select {
		case <-ctx.Done():
			return accessUnknown, ctx.Err()
		case <-ticker.C:
		}
	}
}

func newNativeEventStore() (managedEventStore, error) {
	var store *C.bsbctl_calendar_store
	var nativeError *C.char
	if result := C.bsbctl_calendar_store_new(&store, &nativeError); result != 0 || store == nil {
		return nil, consumeNativeCalendarError("create EventKit store", &nativeError)
	}
	descriptor := int(C.bsbctl_calendar_store_change_fd(store))
	if descriptor < 0 {
		C.bsbctl_calendar_store_free(store)
		return nil, errors.New("create EventKit change stream")
	}
	value := &nativeEventStore{
		store: store, changeFD: os.NewFile(uintptr(descriptor), "bsbctl-calendar-changes"),
		changes: make(chan struct{}, 1), done: make(chan struct{}),
	}
	go value.drainChanges()
	return value, nil
}

func (s *nativeEventStore) AuthorizationStatus() accessStatus {
	return accessStatusFromNative(int(C.bsbctl_calendar_authorization_status()))
}

func (s *nativeEventStore) RequestFullAccess(ctx context.Context) (accessStatus, error) {
	if err := ctx.Err(); err != nil {
		return accessUnknown, err
	}
	s.mu.Lock()
	if s.store == nil {
		s.mu.Unlock()
		return accessUnknown, errors.New("EventKit store is closed")
	}
	var request *C.bsbctl_calendar_authorization_request
	var nativeError *C.char
	result := int(C.bsbctl_calendar_authorization_start(s.store, &request, &nativeError))
	s.mu.Unlock()
	if nativeError != nil {
		return accessUnknown, consumeNativeCalendarError("request full Calendar access", &nativeError)
	}
	if result != 0 || request == nil {
		return accessUnknown, errors.New("start EventKit authorization request")
	}
	return waitForAuthorization(ctx, &nativeAuthorizationRequest{request: request})
}

func (s *nativeEventStore) Events(ctx context.Context, start, end time.Time) ([]calendarEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !end.After(start) {
		return nil, errors.New("EventKit query end must follow start")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return nil, errors.New("EventKit store is closed")
	}
	var result C.bsbctl_calendar_events_result
	code := C.bsbctl_calendar_copy_events(s.store, C.int64_t(start.Unix()), C.int64_t(end.Unix()), &result)
	defer C.bsbctl_calendar_free_events(&result)
	if code != 0 {
		return nil, consumeNativeCalendarError("read EventKit events", &result.error)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items := unsafe.Slice((*C.bsbctl_calendar_event)(unsafe.Pointer(result.items)), int(result.count))
	events := make([]calendarEvent, 0, len(items))
	for _, item := range items {
		events = append(events, calendarEvent{
			CalendarID: C.GoString(item.calendar_id), EventID: C.GoString(item.event_id),
			Title: C.GoString(item.title), URL: C.GoString(item.url),
			Start: time.Unix(int64(item.start_unix), 0).UTC(), End: time.Unix(int64(item.end_unix), 0).UTC(),
			OccurrenceAt: time.Unix(int64(item.occurrence_unix), 0).UTC(),
			AllDay:       item.all_day != 0, Cancelled: item.cancelled != 0,
		})
	}
	return events, nil
}

func (s *nativeEventStore) Calendars(ctx context.Context) ([]calendarInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return nil, errors.New("EventKit store is closed")
	}
	var result C.bsbctl_calendar_list_result
	code := C.bsbctl_calendar_copy_calendars(s.store, &result)
	defer C.bsbctl_calendar_free_calendars(&result)
	if code != 0 {
		return nil, consumeNativeCalendarError("read EventKit calendars", &result.error)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items := unsafe.Slice((*C.bsbctl_calendar_info)(unsafe.Pointer(result.items)), int(result.count))
	calendars := make([]calendarInfo, 0, len(items))
	for _, item := range items {
		calendars = append(calendars, calendarInfo{ID: C.GoString(item.calendar_id), Title: C.GoString(item.title), Source: C.GoString(item.source)})
	}
	return calendars, nil
}

func (s *nativeEventStore) Changes() <-chan struct{} { return s.changes }

func (s *nativeEventStore) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		changeFD := s.changeFD
		s.changeFD = nil
		store := s.store
		s.store = nil
		s.mu.Unlock()
		if changeFD != nil {
			s.closeErr = changeFD.Close()
		}
		if store != nil {
			C.bsbctl_calendar_store_free(store)
		}
		<-s.done
	})
	return s.closeErr
}

func (s *nativeEventStore) drainChanges() {
	defer close(s.done)
	defer close(s.changes)
	buffer := make([]byte, 64)
	for {
		s.mu.Lock()
		file := s.changeFD
		s.mu.Unlock()
		if file == nil {
			return
		}
		count, err := file.Read(buffer)
		if count > 0 {
			select {
			case s.changes <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

type nativeURLOpener struct{}

func (nativeURLOpener) Open(ctx context.Context, rawURL string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value := C.CString(rawURL)
	defer C.free(unsafe.Pointer(value))
	if C.bsbctl_calendar_open_url(value) != 0 {
		return errors.New("system URL opener rejected the meeting URL")
	}
	return ctx.Err()
}

// consumeNativeCalendarError transfers the allocation out of its owner before
// freeing it. Aggregate result cleanup can then safely run on every path.
func consumeNativeCalendarError(operation string, owner **C.char) error {
	nativeError := *owner
	*owner = nil
	if nativeError == nil {
		return errors.New(operation)
	}
	defer C.free(unsafe.Pointer(nativeError))
	message := C.GoString(nativeError)
	if message == "" {
		return errors.New(operation)
	}
	return fmt.Errorf("%s: %s", operation, message)
}
