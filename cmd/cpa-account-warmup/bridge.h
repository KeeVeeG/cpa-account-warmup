#ifndef ACCOUNT_WARMUP_BRIDGE_H
#define ACCOUNT_WARMUP_BRIDGE_H

#include <stdint.h>
#include <stdlib.h>

/* CLIProxyAPI native ABI v1. Ownership never crosses an allocator boundary. */
typedef struct { void *ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void *, const char *, const uint8_t *, size_t, cliproxy_buffer *);
typedef void (*cliproxy_host_free_fn)(void *, size_t);
typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    cliproxy_host_call_fn call;
    cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char *, uint8_t *, size_t, cliproxy_buffer *);
typedef void (*cliproxy_plugin_free_fn)(void *, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char *, uint8_t *, size_t, cliproxy_buffer *);
extern void cliproxyPluginFree(void *, size_t);
extern void cliproxyPluginShutdown(void);
void warmup_set_host(const cliproxy_host_api *host);
int warmup_host_call(const char *method, const uint8_t *request, size_t len, cliproxy_buffer *response);
void warmup_host_free(void *ptr, size_t len);

#endif
