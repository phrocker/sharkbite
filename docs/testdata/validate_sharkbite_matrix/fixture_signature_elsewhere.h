typedef int stale_type;

typedef struct shoal_bytes {
    const char *data;
    unsigned long long len;
} shoal_bytes;

int fixture_signature(
    shoal_bytes row,
    const char *name,
    int timeout_ms);
