#define _DARWIN_C_SOURCE
#include "bridge.h"
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

/* The Go runtime lives in this executable, never in the host's address space. */
extern const unsigned char helper_start[] __asm("section$start$__DATA$__warmup_helper");
extern const unsigned char helper_end[] __asm("section$end$__DATA$__warmup_helper");

enum { frame_magic = 0x57525043, max_payload = 16 << 20 };
enum { plugin_request = 1, plugin_response = 2, host_request = 3, host_response = 4, shutdown_request = 5 };

struct pending_call {
    uint64_t id;
    int done;
    uint32_t status;
    void *data;
    size_t length;
    pthread_cond_t ready;
    struct pending_call *next;
};

struct host_call {
    pthread_t thread;
    uint64_t id;
    unsigned char *payload;
    size_t length;
    int done;
    struct host_call *next;
};

static pthread_mutex_t lifecycle = PTHREAD_MUTEX_INITIALIZER;
static struct {
    pthread_mutex_t lock;
    pthread_mutex_t writer;
    pthread_cond_t idle;
    int socket;
    int running;
    unsigned active_calls;
    uint64_t sequence;
    pid_t child;
    pthread_t reader;
    int reader_started;
    struct pending_call *pending;
    struct host_call *workers;
    cliproxy_host_api host;
    char directory[PATH_MAX];
    char executable[PATH_MAX];
} state = {
    .lock = PTHREAD_MUTEX_INITIALIZER,
    .writer = PTHREAD_MUTEX_INITIALIZER,
    .idle = PTHREAD_COND_INITIALIZER,
    .socket = -1
};

static void stop_child(void);
static void remove_helper(void);

static void put32(unsigned char *out, uint32_t value) {
    for (unsigned i = 0; i < 4; i++) out[i] = (unsigned char)(value >> (i * 8));
}

static uint32_t get32(const unsigned char *in) {
    uint32_t value = 0;
    for (unsigned i = 0; i < 4; i++) value |= (uint32_t)in[i] << (i * 8);
    return value;
}

static int write_all(int descriptor, const void *data, size_t length) {
    const unsigned char *cursor = data;
    while (length) {
        ssize_t written = write(descriptor, cursor, length);
        if (written < 0 && errno == EINTR) continue;
        if (written <= 0) return 0;
        cursor += written;
        length -= (size_t)written;
    }
    return 1;
}

static int read_all(int descriptor, void *data, size_t length) {
    unsigned char *cursor = data;
    while (length) {
        ssize_t received = read(descriptor, cursor, length);
        if (received < 0 && errno == EINTR) continue;
        if (received <= 0) return 0;
        cursor += received;
        length -= (size_t)received;
    }
    return 1;
}

static void disconnect_helper(void) {
    pthread_mutex_lock(&state.lock);
    state.running = 0;
    if (state.socket >= 0) shutdown(state.socket, SHUT_RDWR);
    for (struct pending_call *call = state.pending; call; call = call->next) {
        if (!call->done) {
            call->done = 1;
            call->status = 1;
            pthread_cond_signal(&call->ready);
        }
    }
    pthread_mutex_unlock(&state.lock);
}

static int send_frame(uint32_t kind, uint64_t id, uint32_t status, const void *data, size_t length) {
    unsigned char header[24];
    if (length > max_payload || (length && !data)) return 0;
    put32(header, frame_magic);
    put32(header + 4, kind);
    put32(header + 8, (uint32_t)id);
    put32(header + 12, (uint32_t)(id >> 32));
    put32(header + 16, status);
    put32(header + 20, (uint32_t)length);
    pthread_mutex_lock(&state.writer);
    pthread_mutex_lock(&state.lock);
    int descriptor = state.running ? state.socket : -1;
    pthread_mutex_unlock(&state.lock);
    int ok = descriptor >= 0 && write_all(descriptor, header, sizeof(header)) && write_all(descriptor, data, length);
    pthread_mutex_unlock(&state.writer);
    if (!ok) disconnect_helper();
    return ok;
}

static void *call_host(void *argument) {
    struct host_call *call = argument;
    char *method = (char *)call->payload;
    size_t method_length = strlen(method) + 1;
    cliproxy_buffer response = {0};
    int status = state.host.call(state.host.host_ctx, method, call->payload + method_length,
                                call->length - method_length, &response);
    if (response.len > max_payload || (response.len && !response.ptr)) {
        send_frame(host_response, call->id, 1, NULL, 0);
    } else {
        send_frame(host_response, call->id, (uint32_t)status, response.ptr, response.len);
    }
    if (response.ptr) state.host.free_buffer(response.ptr, response.len);
    pthread_mutex_lock(&state.lock);
    call->done = 1;
    pthread_mutex_unlock(&state.lock);
    return NULL;
}

/* Join completed workers before freeing them; shutdown must not leave code on
   another thread's stack when the host unloads this library. */
static void reap_workers(int all) {
    for (;;) {
        pthread_mutex_lock(&state.lock);
        struct host_call **cursor = &state.workers;
        while (*cursor && !all && !(*cursor)->done) cursor = &(*cursor)->next;
        struct host_call *call = *cursor;
        if (call) *cursor = call->next;
        pthread_mutex_unlock(&state.lock);
        if (!call) return;
        pthread_join(call->thread, NULL);
        free(call->payload);
        free(call);
    }
}

static void *read_frames(void *unused) {
    (void)unused;
    for (;;) {
        unsigned char header[24];
        if (!read_all(state.socket, header, sizeof(header))) break;
        uint32_t kind = get32(header + 4);
        uint64_t id = get32(header + 8) | ((uint64_t)get32(header + 12) << 32);
        uint32_t status = get32(header + 16);
        uint32_t length = get32(header + 20);
        if (get32(header) != frame_magic || !id || length > max_payload ||
            (kind != plugin_response && kind != host_request) || (kind == host_request && status)) break;
        unsigned char *payload = length ? malloc(length) : NULL;
        if (length && (!payload || !read_all(state.socket, payload, length))) {
            free(payload);
            break;
        }
        if (kind == plugin_response) {
            pthread_mutex_lock(&state.lock);
            struct pending_call *call = state.pending;
            while (call && call->id != id) call = call->next;
            if (!call || call->done) {
                pthread_mutex_unlock(&state.lock);
                free(payload);
                break;
            }
            call->data = payload;
            call->length = length;
            call->status = status;
            call->done = 1;
            pthread_cond_signal(&call->ready);
            pthread_mutex_unlock(&state.lock);
        } else {
            unsigned char *separator = length ? memchr(payload, 0, length) : NULL;
            if (!separator || separator == payload || separator - payload > 1024) {
                free(payload);
                break;
            }
            struct host_call *call = calloc(1, sizeof(*call));
            if (!call) {
                free(payload);
                break;
            }
            call->id = id;
            call->payload = payload;
            call->length = length;
            pthread_mutex_lock(&state.lock);
            int duplicate = 0;
            for (struct host_call *other = state.workers; other; other = other->next) {
                if (other->id == id && !other->done) duplicate = 1;
            }
            int error = duplicate ? EINVAL : pthread_create(&call->thread, NULL, call_host, call);
            if (!error) {
                call->next = state.workers;
                state.workers = call;
            }
            pthread_mutex_unlock(&state.lock);
            if (error) {
                free(payload);
                free(call);
                break;
            }
        }
        reap_workers(0);
    }
    disconnect_helper();
    stop_child();
    remove_helper();
    return NULL;
}

static int exchange(uint32_t kind, const void *payload, size_t length, cliproxy_buffer *response) {
    response->ptr = NULL;
    response->len = 0;
    struct pending_call call = {0};
    if (pthread_cond_init(&call.ready, NULL)) return 1;
    pthread_mutex_lock(&state.lock);
    if (!state.running) {
        pthread_mutex_unlock(&state.lock);
        pthread_cond_destroy(&call.ready);
        return 1;
    }
    state.active_calls++;
    call.id = ++state.sequence;
    if (!call.id) call.id = ++state.sequence;
    call.next = state.pending;
    state.pending = &call;
    pthread_mutex_unlock(&state.lock);
    send_frame(kind, call.id, 0, payload, length);
    pthread_mutex_lock(&state.lock);
    while (!call.done) pthread_cond_wait(&call.ready, &state.lock);
    struct pending_call **cursor = &state.pending;
    while (*cursor != &call) cursor = &(*cursor)->next;
    *cursor = call.next;
    response->ptr = call.data;
    response->len = call.length;
    state.active_calls--;
    pthread_cond_broadcast(&state.idle);
    pthread_mutex_unlock(&state.lock);
    pthread_cond_destroy(&call.ready);
    return call.status ? 1 : 0;
}

static int plugin_call(char *method, uint8_t *request, size_t length, cliproxy_buffer *response) {
    if (!response) return 1;
    response->ptr = NULL;
    response->len = 0;
    if (!method || (length && !request)) return 1;
    size_t method_length = strnlen(method, 1025);
    if (!method_length || method_length > 1024 || length > max_payload - method_length - 1) return 1;
    size_t total = method_length + 1 + length;
    unsigned char *payload = malloc(total);
    if (!payload) return 1;
    memcpy(payload, method, method_length + 1);
    if (length) memcpy(payload + method_length + 1, request, length);
    int status = exchange(plugin_request, payload, total, response);
    free(payload);
    return status;
}

static void plugin_free(void *pointer, size_t length) {
    (void)length;
    free(pointer);
}

static void remove_helper(void) {
    if (*state.executable) unlink(state.executable);
    if (*state.directory) rmdir(state.directory);
    *state.executable = 0;
    *state.directory = 0;
}

static void stop_child(void) {
    if (state.child <= 0) return;
    for (unsigned attempt = 0; attempt < 100; attempt++) {
        pid_t result = waitpid(state.child, NULL, WNOHANG);
        if (result == state.child || (result < 0 && errno == ECHILD)) {
            state.child = 0;
            return;
        }
        if (result < 0 && errno != EINTR) break;
        struct timespec delay = { .tv_nsec = 10000000 };
        nanosleep(&delay, NULL);
    }
    kill(state.child, SIGKILL);
    while (waitpid(state.child, NULL, 0) < 0 && errno == EINTR) {}
    state.child = 0;
}

static void shutdown_locked(void) {
    if (!state.reader_started) return;
    cliproxy_buffer response = {0};
    exchange(shutdown_request, NULL, 0, &response);
    free(response.ptr);
    disconnect_helper();
    pthread_join(state.reader, NULL);
    state.reader_started = 0;
    reap_workers(1);
    pthread_mutex_lock(&state.lock);
    while (state.active_calls) pthread_cond_wait(&state.idle, &state.lock);
    pthread_mutex_unlock(&state.lock);
    pthread_mutex_lock(&state.writer);
    close(state.socket);
    state.socket = -1;
    pthread_mutex_unlock(&state.writer);
    stop_child();
    remove_helper();
    memset(&state.host, 0, sizeof(state.host));
}

static void plugin_shutdown(void) {
    pthread_mutex_lock(&lifecycle);
    shutdown_locked();
    pthread_mutex_unlock(&lifecycle);
}

static int extract_helper(void) {
    const char *temporary = getenv("TMPDIR");
    if (!temporary || !*temporary) temporary = "/tmp";
    int length = snprintf(state.directory, sizeof(state.directory), "%s/cpa-account-warmup.XXXXXX", temporary);
    if (length < 0 || (size_t)length >= sizeof(state.directory) || !mkdtemp(state.directory)) {
        *state.directory = 0;
        return 0;
    }
    length = snprintf(state.executable, sizeof(state.executable), "%s/helper", state.directory);
    if (length < 0 || (size_t)length >= sizeof(state.executable)) return 0;
    int descriptor = open(state.executable, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0700);
    if (descriptor < 0) return 0;
    size_t size = (size_t)((uintptr_t)helper_end - (uintptr_t)helper_start);
    int ok = size && write_all(descriptor, helper_start, size) && fchmod(descriptor, 0700) == 0;
    if (close(descriptor)) ok = 0;
    return ok;
}

static int start_helper(void) {
    int sockets[2];
    if (socketpair(AF_UNIX, SOCK_STREAM, 0, sockets)) return 0;
    for (unsigned i = 0; i < 2; i++) {
        if (sockets[i] < 4) {
            int replacement = fcntl(sockets[i], F_DUPFD_CLOEXEC, 4);
            if (replacement < 0) { close(sockets[0]); close(sockets[1]); return 0; }
            close(sockets[i]);
            sockets[i] = replacement;
        }
        int enabled = 1;
        if (fcntl(sockets[i], F_SETFD, FD_CLOEXEC) ||
            setsockopt(sockets[i], SOL_SOCKET, SO_NOSIGPIPE, &enabled, sizeof(enabled))) {
            close(sockets[0]); close(sockets[1]); return 0;
        }
    }
    posix_spawn_file_actions_t actions;
    posix_spawnattr_t attributes;
    if (posix_spawn_file_actions_init(&actions)) { close(sockets[0]); close(sockets[1]); return 0; }
    if (posix_spawnattr_init(&attributes)) {
        posix_spawn_file_actions_destroy(&actions);
        close(sockets[0]); close(sockets[1]); return 0;
    }
    int error = 0;
    for (int descriptor = 0; descriptor < 3 && !error; descriptor++) {
        error = posix_spawn_file_actions_addopen(&actions, descriptor, "/dev/null", O_RDWR, 0600);
    }
    if (!error) error = posix_spawn_file_actions_adddup2(&actions, sockets[1], 3);
    if (!error) error = posix_spawn_file_actions_addclose(&actions, sockets[0]);
    if (!error) error = posix_spawn_file_actions_addclose(&actions, sockets[1]);
    if (!error) error = posix_spawnattr_setflags(&attributes, POSIX_SPAWN_CLOEXEC_DEFAULT);
    char *arguments[] = { state.executable, NULL };
    char *environment[] = { "PATH=/usr/bin:/bin", NULL };
    if (!error) error = posix_spawn(&state.child, state.executable, &actions, &attributes, arguments, environment);
    posix_spawnattr_destroy(&attributes);
    posix_spawn_file_actions_destroy(&actions);
    close(sockets[1]);
    if (error) { close(sockets[0]); state.child = 0; return 0; }
    state.socket = sockets[0];
    state.running = 1;
    if (pthread_create(&state.reader, NULL, read_frames, NULL)) {
        disconnect_helper();
        close(state.socket);
        state.socket = -1;
        stop_child();
        return 0;
    }
    state.reader_started = 1;
    return 1;
}

__attribute__((visibility("default")))
int cliproxy_plugin_init(const cliproxy_host_api *host, cliproxy_plugin_api *api) {
    if (!host || !api || host->abi_version != 1 || !host->call || !host->free_buffer) return 1;
    pthread_mutex_lock(&lifecycle);
    shutdown_locked();
    state.host = *host;
    int ok = extract_helper() && start_helper();
    if (ok) {
        api->abi_version = 1;
        api->call = plugin_call;
        api->free_buffer = plugin_free;
        api->shutdown = plugin_shutdown;
    } else {
        remove_helper();
        memset(&state.host, 0, sizeof(state.host));
    }
    pthread_mutex_unlock(&lifecycle);
    return ok ? 0 : 1;
}
