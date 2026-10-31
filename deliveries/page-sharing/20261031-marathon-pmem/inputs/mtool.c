/* mtool.c — marathon tool simulator (guest, musl dynamic).
 *
 * argv: mb dur corpus
 *   mb     — anon MB to allocate+touch (rustc scratch / grep buffers)
 *   dur    — legacy trace duration, accepted but never used to pad execution
 *   corpus — file to stream-read (page-cache phase for read-class tools)
 *
 * Under LD_PRELOAD=libarena_shim.so (native mode) every >=64KB allocation
 * lands in the tool's tide_arena; exit hands the arena id via
 * /root/.arena_last_id and the host punches its extents (destroy(T)).
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>

int main(int argc, char **argv)
{
    if (argc == 2 && !strcmp(argv[1], "--policy")) { puts("fixed-work-no-padding-v1"); return 0; }
    if (argc < 4) { fputs("usage: mtool MB DUR CORPUS\n", stderr); return 2; }
    long mb = atol(argv[1]);
    double dur = atof(argv[2]);
    const char *corpus = argv[3];
    const char *kind = argc > 4 ? argv[4] : "read";
    if (mb < 0 || dur < 0) return 2;
    /* Work is fixed, independent of elapsed time and trace duration. */

    /* anon allocation burst (rustc scratch / tool heap) */
    char **chunks = NULL;
    long *sizes = NULL;
    long nch = 0, cap = 64;
    chunks = malloc(cap * sizeof(char *));
    sizes = malloc(cap * sizeof(long));
    long remain = mb << 20;
    long CH = 32 << 20; /* 32MB chunks: exercise malloc + mmap paths */
    while (remain > 0) {
        long sz = remain < CH ? remain : CH;
        if (nch == cap) {
            cap *= 2;
            chunks = realloc(chunks, cap * sizeof(char *));
            sizes = realloc(sizes, cap * sizeof(long));
        }
        char *p = malloc(sz);
        if (!p) { fprintf(stderr, "mtool: OOM at %ldMB\n", mb - (remain >> 20)); return 1; }
        for (long o = 0; o < sz; o += 4096) ((volatile char *)p)[o] = 0xCD;
        chunks[nch] = p;
        sizes[nch] = sz;
        nch++;
        remain -= sz;
    }

    /* Only read-class tools stream files. A compiler touches its heap;
     * it does not repeatedly read the unrelated 512MB grep corpus. */
    char buf[1 << 16];
    unsigned long long read_bytes = 0;
    if (!strcmp(kind, "read") || !strcmp(kind, "scan")) {
        int fd = open(corpus, O_RDONLY);
        if (fd < 0) { perror("corpus"); return 3; }
        /* One stream per invocation, not an unbounded read loop. */
        while (1) {
            ssize_t n = read(fd, buf, sizeof buf);
            if (n == 0) break;
            if (n < 0 && errno != EINTR) { perror("read"); return 3; }
            if (n > 0) read_bytes += (unsigned long long)n;
        }
        close(fd);
    }
    /* touch every chunk once more (finish deterministic work);
     * bound by the chunk's own size — a short tail chunk must not be
     * touched past its end (arena VA space beyond may be unmapped) */
    for (long i = 0; i < nch; i++)
        for (long o = 0; o < sizes[i]; o += 65536)
            ((volatile char *)chunks[i])[o] ^= 0x01;

    printf("{\"allocated_bytes\":%llu,\"read_bytes\":%llu,\"policy\":\"fixed-work-no-padding-v1\"}\n",
           (unsigned long long)mb << 20, read_bytes);
    return 0;
}
