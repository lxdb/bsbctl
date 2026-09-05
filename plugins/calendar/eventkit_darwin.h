#ifndef BSBCTL_EVENTKIT_DARWIN_H
#define BSBCTL_EVENTKIT_DARWIN_H

#include <stddef.h>
#include <stdint.h>

typedef struct bsbctl_calendar_store bsbctl_calendar_store;
typedef struct bsbctl_calendar_authorization_request bsbctl_calendar_authorization_request;

enum {
    BSBCTL_CALENDAR_ACCESS_NOT_DETERMINED = 0,
    BSBCTL_CALENDAR_ACCESS_RESTRICTED = 1,
    BSBCTL_CALENDAR_ACCESS_DENIED = 2,
    BSBCTL_CALENDAR_ACCESS_WRITE_ONLY = 3,
    BSBCTL_CALENDAR_ACCESS_FULL = 4
};

typedef struct {
    char *calendar_id;
    char *event_id;
    char *title;
    char *url;
    int64_t start_unix;
    int64_t end_unix;
    int64_t occurrence_unix;
    int all_day;
    int cancelled;
} bsbctl_calendar_event;

typedef struct {
    bsbctl_calendar_event *items;
    size_t count;
    char *error;
} bsbctl_calendar_events_result;

typedef struct {
    char *calendar_id;
    char *title;
    char *source;
} bsbctl_calendar_info;

typedef struct {
    bsbctl_calendar_info *items;
    size_t count;
    char *error;
} bsbctl_calendar_list_result;

int bsbctl_calendar_store_new(bsbctl_calendar_store **output, char **error_output);
void bsbctl_calendar_store_free(bsbctl_calendar_store *store);
int bsbctl_calendar_store_change_fd(bsbctl_calendar_store *store);
int bsbctl_calendar_authorization_status(void);
int bsbctl_calendar_authorization_start(
    bsbctl_calendar_store *store,
    bsbctl_calendar_authorization_request **output,
    char **error_output);
int bsbctl_calendar_authorization_poll(
    bsbctl_calendar_authorization_request *request,
    int *status_output,
    char **error_output);
void bsbctl_calendar_authorization_release(bsbctl_calendar_authorization_request *request);
int bsbctl_calendar_copy_events(
    bsbctl_calendar_store *store,
    int64_t start_unix,
    int64_t end_unix,
    bsbctl_calendar_events_result *result);
void bsbctl_calendar_free_events(bsbctl_calendar_events_result *result);
int bsbctl_calendar_copy_calendars(bsbctl_calendar_store *store, bsbctl_calendar_list_result *result);
void bsbctl_calendar_free_calendars(bsbctl_calendar_list_result *result);
int bsbctl_calendar_open_url(const char *raw_url);

#endif
