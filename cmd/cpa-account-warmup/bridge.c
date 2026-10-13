#include "bridge.h"
#include <string.h>

static cliproxy_host_api host_api;

void warmup_set_host(const cliproxy_host_api *host) {
    if (host) host_api = *host;
    else memset(&host_api, 0, sizeof(host_api));
}

int warmup_host_call(const char *method, const uint8_t *request, size_t len, cliproxy_buffer *response) {
    if (!host_api.call) return 1;
    return host_api.call(host_api.host_ctx, method, request, len, response);
}

void warmup_host_free(void *ptr, size_t len) {
    if (ptr && host_api.free_buffer) host_api.free_buffer(ptr, len);
}
