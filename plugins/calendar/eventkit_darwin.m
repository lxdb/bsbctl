#import <AppKit/AppKit.h>
#import <EventKit/EventKit.h>
#import <Foundation/Foundation.h>

#include "eventkit_darwin.h"

#include <fcntl.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

struct bsbctl_calendar_store {
    EKEventStore *event_store;
    id observer;
    int change_read_fd;
    int change_write_fd;
};

struct bsbctl_calendar_authorization_request {
    pthread_mutex_t mutex;
    EKEventStore *event_store;
    char *error;
    int status;
    int completed;
    int references;
};

static const NSUInteger bsbctl_calendar_max_events = 4096;
static const NSUInteger bsbctl_calendar_max_calendars = 256;

static char *bsbctl_calendar_copy_string(NSString *value) {
    const char *utf8 = value == nil ? "" : [value UTF8String];
    return strdup(utf8 == NULL ? "" : utf8);
}

static void bsbctl_calendar_set_error(char **output, NSString *message) {
    if (output != NULL) {
        *output = bsbctl_calendar_copy_string(message == nil ? @"Calendar operation failed" : message);
    }
}

static int bsbctl_calendar_map_authorization(EKAuthorizationStatus status) {
    switch (status) {
        case EKAuthorizationStatusNotDetermined:
            return BSBCTL_CALENDAR_ACCESS_NOT_DETERMINED;
        case EKAuthorizationStatusRestricted:
            return BSBCTL_CALENDAR_ACCESS_RESTRICTED;
        case EKAuthorizationStatusDenied:
            return BSBCTL_CALENDAR_ACCESS_DENIED;
        case EKAuthorizationStatusWriteOnly:
            return BSBCTL_CALENDAR_ACCESS_WRITE_ONLY;
        case EKAuthorizationStatusFullAccess:
            return BSBCTL_CALENDAR_ACCESS_FULL;
        default:
            return -1;
    }
}

static void bsbctl_calendar_close_pipe(int *descriptor) {
    if (*descriptor >= 0) {
        close(*descriptor);
        *descriptor = -1;
    }
}

int bsbctl_calendar_store_new(bsbctl_calendar_store **output, char **error_output) {
    if (output == NULL) {
        bsbctl_calendar_set_error(error_output, @"Calendar store output is unavailable");
        return -1;
    }
    *output = NULL;
    if (error_output != NULL) *error_output = NULL;
    @autoreleasepool {
        bsbctl_calendar_store *store = calloc(1, sizeof(*store));
        if (store == NULL) {
            bsbctl_calendar_set_error(error_output, @"Allocate Calendar store");
            return -1;
        }
        store->change_read_fd = -1;
        store->change_write_fd = -1;
        int descriptors[2];
        if (pipe(descriptors) != 0) {
            free(store);
            bsbctl_calendar_set_error(error_output, @"Create Calendar change pipe");
            return -1;
        }
        store->change_read_fd = descriptors[0];
        store->change_write_fd = descriptors[1];
        fcntl(store->change_read_fd, F_SETFD, FD_CLOEXEC);
        fcntl(store->change_write_fd, F_SETFD, FD_CLOEXEC);
        int flags = fcntl(store->change_write_fd, F_GETFL, 0);
        if (flags >= 0) fcntl(store->change_write_fd, F_SETFL, flags | O_NONBLOCK);

        store->event_store = [[EKEventStore alloc] init];
        if (store->event_store == nil) {
            bsbctl_calendar_close_pipe(&store->change_read_fd);
            bsbctl_calendar_close_pipe(&store->change_write_fd);
            free(store);
            bsbctl_calendar_set_error(error_output, @"Create EventKit store");
            return -1;
        }
        int change_write_fd = store->change_write_fd;
        id observer = [[NSNotificationCenter defaultCenter]
            addObserverForName:EKEventStoreChangedNotification
                        object:store->event_store
                         queue:nil
                    usingBlock:^(NSNotification *notification) {
                        (void)notification;
                        const uint8_t signal = 1;
                        (void)write(change_write_fd, &signal, sizeof(signal));
                    }];
        store->observer = [observer retain];
        *output = store;
        return 0;
    }
}

void bsbctl_calendar_store_free(bsbctl_calendar_store *store) {
    if (store == NULL) return;
    @autoreleasepool {
        if (store->observer != nil) {
            [[NSNotificationCenter defaultCenter] removeObserver:store->observer];
            [store->observer release];
            store->observer = nil;
        }
        [store->event_store release];
        store->event_store = nil;
        bsbctl_calendar_close_pipe(&store->change_read_fd);
        bsbctl_calendar_close_pipe(&store->change_write_fd);
        free(store);
    }
}

int bsbctl_calendar_store_change_fd(bsbctl_calendar_store *store) {
    if (store == NULL || store->change_read_fd < 0) return -1;
    int descriptor = dup(store->change_read_fd);
    if (descriptor >= 0) fcntl(descriptor, F_SETFD, FD_CLOEXEC);
    return descriptor;
}

int bsbctl_calendar_authorization_status(void) {
    return bsbctl_calendar_map_authorization([EKEventStore authorizationStatusForEntityType:EKEntityTypeEvent]);
}

void bsbctl_calendar_authorization_release(bsbctl_calendar_authorization_request *request) {
    if (request == NULL) return;
    pthread_mutex_lock(&request->mutex);
    request->references--;
    int destroy = request->references == 0;
    pthread_mutex_unlock(&request->mutex);
    if (!destroy) return;
    [request->event_store release];
    free(request->error);
    pthread_mutex_destroy(&request->mutex);
    free(request);
}

int bsbctl_calendar_authorization_start(
    bsbctl_calendar_store *store,
    bsbctl_calendar_authorization_request **output,
    char **error_output) {
    if (output != NULL) *output = NULL;
    if (error_output != NULL) *error_output = NULL;
    if (output == NULL || store == NULL || store->event_store == nil) {
        bsbctl_calendar_set_error(error_output, @"EventKit store is unavailable");
        return -1;
    }
    @autoreleasepool {
        if (@available(macOS 14.0, *)) {
            bsbctl_calendar_authorization_request *request = calloc(1, sizeof(*request));
            if (request == NULL || pthread_mutex_init(&request->mutex, NULL) != 0) {
                free(request);
                bsbctl_calendar_set_error(error_output, @"Allocate Calendar authorization request");
                return -1;
            }
            request->event_store = [store->event_store retain];
            request->status = -1;
            request->references = 2;
            void (^completion)(BOOL, NSError *) = ^(BOOL granted, NSError *error) {
                (void)granted;
                pthread_mutex_lock(&request->mutex);
                if (error != nil) request->error = bsbctl_calendar_copy_string([error localizedDescription]);
                request->status = bsbctl_calendar_authorization_status();
                request->completed = 1;
                pthread_mutex_unlock(&request->mutex);
                bsbctl_calendar_authorization_release(request);
            };
            [request->event_store requestFullAccessToEventsWithCompletion:completion];
            *output = request;
            return 0;
        }
        bsbctl_calendar_set_error(error_output, @"Calendar full access requires macOS 14 or later");
        return -1;
    }
}

int bsbctl_calendar_authorization_poll(
    bsbctl_calendar_authorization_request *request,
    int *status_output,
    char **error_output) {
	if (status_output != NULL) *status_output = -1;
	if (error_output != NULL) *error_output = NULL;
	if (request == NULL || status_output == NULL) return -1;
	pthread_mutex_lock(&request->mutex);
	int completed = request->completed;
	if (completed) {
		*status_output = request->status;
		if (request->error != NULL && error_output != NULL) *error_output = strdup(request->error);
	}
	pthread_mutex_unlock(&request->mutex);
	return completed;
}

void bsbctl_calendar_free_events(bsbctl_calendar_events_result *result) {
    if (result == NULL) return;
    if (result->items != NULL) {
        for (size_t index = 0; index < result->count; index++) {
            free(result->items[index].calendar_id);
            free(result->items[index].event_id);
            free(result->items[index].title);
            free(result->items[index].url);
        }
        free(result->items);
    }
    free(result->error);
    result->items = NULL;
    result->count = 0;
    result->error = NULL;
}

void bsbctl_calendar_free_calendars(bsbctl_calendar_list_result *result) {
    if (result == NULL) return;
    if (result->items != NULL) {
        for (size_t index = 0; index < result->count; index++) {
            free(result->items[index].calendar_id);
            free(result->items[index].title);
            free(result->items[index].source);
        }
        free(result->items);
    }
    free(result->error);
    result->items = NULL;
    result->count = 0;
    result->error = NULL;
}

int bsbctl_calendar_copy_calendars(bsbctl_calendar_store *store, bsbctl_calendar_list_result *result) {
    if (result == NULL) return -1;
    memset(result, 0, sizeof(*result));
    if (store == NULL || store->event_store == nil) {
        result->error = bsbctl_calendar_copy_string(@"EventKit store is unavailable");
        return -1;
    }
    @autoreleasepool {
        NSArray<EKCalendar *> *calendars = [store->event_store calendarsForEntityType:EKEntityTypeEvent];
        if (calendars == nil) {
            result->error = bsbctl_calendar_copy_string(@"EventKit calendar query failed");
            return -1;
        }
        if ([calendars count] > bsbctl_calendar_max_calendars) {
            result->error = bsbctl_calendar_copy_string(@"EventKit calendar query exceeded the calendar limit");
            return -1;
        }
        result->count = (size_t)[calendars count];
        if (result->count == 0) return 0;
        result->items = calloc(result->count, sizeof(*result->items));
        if (result->items == NULL) {
            result->count = 0;
            result->error = bsbctl_calendar_copy_string(@"Allocate EventKit calendar results");
            return -1;
        }
        for (size_t index = 0; index < result->count; index++) {
            EKCalendar *calendar = calendars[index];
            result->items[index].calendar_id = bsbctl_calendar_copy_string(calendar.calendarIdentifier);
            result->items[index].title = bsbctl_calendar_copy_string(calendar.title);
            result->items[index].source = bsbctl_calendar_copy_string(calendar.source.title);
            if (result->items[index].calendar_id == NULL || result->items[index].title == NULL || result->items[index].source == NULL) {
                result->error = bsbctl_calendar_copy_string(@"Allocate EventKit calendar fields");
                return -1;
            }
        }
        return 0;
    }
}

int bsbctl_calendar_copy_events(
    bsbctl_calendar_store *store,
    int64_t start_unix,
    int64_t end_unix,
    bsbctl_calendar_events_result *result) {
    if (result == NULL) return -1;
    memset(result, 0, sizeof(*result));
    if (store == NULL || store->event_store == nil || end_unix <= start_unix) {
        result->error = bsbctl_calendar_copy_string(@"Invalid EventKit query");
        return -1;
    }
    @autoreleasepool {
        NSDate *start = [NSDate dateWithTimeIntervalSince1970:(NSTimeInterval)start_unix];
        NSDate *end = [NSDate dateWithTimeIntervalSince1970:(NSTimeInterval)end_unix];
        NSPredicate *predicate = [store->event_store predicateForEventsWithStartDate:start endDate:end calendars:nil];
        NSArray<EKEvent *> *events = [store->event_store eventsMatchingPredicate:predicate];
        if (events == nil) {
            result->error = bsbctl_calendar_copy_string(@"EventKit query failed");
            return -1;
        }
        if ([events count] > bsbctl_calendar_max_events) {
            result->error = bsbctl_calendar_copy_string(@"EventKit query exceeded the event limit");
            return -1;
        }
        result->count = (size_t)[events count];
        if (result->count == 0) return 0;
        result->items = calloc(result->count, sizeof(*result->items));
        if (result->items == NULL) {
            result->count = 0;
            result->error = bsbctl_calendar_copy_string(@"Allocate EventKit results");
            return -1;
        }
        size_t output_index = 0;
        for (size_t index = 0; index < result->count; index++) {
            EKEvent *event = events[index];
            NSString *event_id = event.eventIdentifier;
            if ([event_id length] == 0) event_id = event.calendarItemExternalIdentifier;
            NSString *calendar_id = event.calendar.calendarIdentifier;
            if ([event_id length] == 0 || [calendar_id length] == 0) continue;
            bsbctl_calendar_event *output = &result->items[output_index++];
            output->calendar_id = bsbctl_calendar_copy_string(calendar_id);
            output->event_id = bsbctl_calendar_copy_string(event_id);
            output->title = bsbctl_calendar_copy_string(event.title);
            output->url = bsbctl_calendar_copy_string(event.URL.absoluteString);
            if (output->calendar_id == NULL || output->event_id == NULL || output->title == NULL || output->url == NULL) {
                result->error = bsbctl_calendar_copy_string(@"Allocate EventKit event fields");
                return -1;
            }
            output->start_unix = (int64_t)[event.startDate timeIntervalSince1970];
            output->end_unix = (int64_t)[event.endDate timeIntervalSince1970];
            NSDate *occurrence = event.occurrenceDate ?: event.startDate;
            output->occurrence_unix = (int64_t)[occurrence timeIntervalSince1970];
            output->all_day = event.isAllDay ? 1 : 0;
            output->cancelled = event.status == EKEventStatusCanceled ? 1 : 0;
        }
        result->count = output_index;
        return 0;
    }
}

int bsbctl_calendar_open_url(const char *raw_url) {
    if (raw_url == NULL) return -1;
    @autoreleasepool {
        NSString *value = [NSString stringWithUTF8String:raw_url];
        NSURL *url = value == nil ? nil : [NSURL URLWithString:value];
        if (url == nil) return -1;
        return [[NSWorkspace sharedWorkspace] openURL:url] ? 0 : -1;
    }
}
