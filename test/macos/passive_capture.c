// SPDX-License-Identifier: AGPL-3.0-only
// Passive libpcap-to-disk capture. No packet decoding or payload logging.
#include <sys/types.h>
#include <sys/ioctl.h>
#ifdef __APPLE__
// Native BPF must precede libpcap's compatibility declarations.
#include <net/bpf.h>
#endif
#include <pcap/pcap.h>
#include <sys/resource.h>
#include <sys/select.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <fcntl.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <errno.h>

static volatile sig_atomic_t stop_requested;
static uint64_t packets, packet_bytes, file_bytes = 24, truncated, max_bytes;
static int failed;
static pcap_dumper_t *dump;

static double now(void) {
    struct timespec t;
    if (clock_gettime(CLOCK_MONOTONIC, &t)) exit(90);
    return t.tv_sec + t.tv_nsec / 1e9;
}
static void stop(int sig) { (void)sig; stop_requested = 1; }
static uint64_t number(const char *s) {
    char *end;
    errno = 0;
    unsigned long long n = strtoull(s, &end, 10);
    if (errno || !*s || *s == '-' || *end) exit(91);
    return n;
}
static FILE *fresh(const char *path) {
    int fd = open(path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0600);
    if (fd < 0) return NULL;
    FILE *f = fdopen(fd, "w");
    if (!f) close(fd);
    return f;
}
static FILE *metadata(const char *dir, const char *name) {
    char path[4096];
    if (snprintf(path, sizeof path, "%s/%s", dir, name) >= (int)sizeof path) return NULL;
    return fresh(path);
}
static void packet(unsigned char *user, const struct pcap_pkthdr *h, const unsigned char *bytes) {
    (void)user;
    if (failed) return;
    if (file_bytes + 16 + h->caplen > max_bytes) { failed = 21; return; }
    pcap_dump((unsigned char *)dump, h, bytes);
    packets++;
    packet_bytes += h->caplen;
    file_bytes += 16 + h->caplen;
    if (h->caplen != h->len) truncated++;
}

int main(int argc, char **argv) {
    // capture IFACE PORT|smb BUFFER_BYTES RAW META_DIR MAX_BYTES MAX_SECONDS MIN_FREE_BYTES
    if (argc != 10 || strcmp(argv[1], "capture")) return 2;
    umask(077);
    unsigned requested = (unsigned)number(argv[4]);
    max_bytes = number(argv[7]);
    unsigned seconds = (unsigned)number(argv[8]);
    uint64_t min_free = number(argv[9]);
    if (requested < 4096 || requested > 32U*1024*1024 || seconds < 1 || seconds > 1800 ||
        max_bytes < 24 || max_bytes > 48ULL*1024*1024*1024 || min_free < 1024*1024) return 3;
    struct stat st;
    if (lstat(argv[6], &st) || !S_ISDIR(st.st_mode) || (st.st_mode & 077)) return 4;
    char filter[128];
    if (!strcmp(argv[3], "smb")) strcpy(filter, "tcp and (port 445 or port 1445)");
    else {
        uint64_t port = number(argv[3]);
        if (!port || port > 65535) return 5;
        snprintf(filter, sizeof filter, "tcp port %u", (unsigned)port);
    }
    char err[PCAP_ERRBUF_SIZE]; // Never publish provider error strings.
    pcap_t *p = pcap_create(argv[2], err);
    if (!p) return 6;
    if (pcap_set_snaplen(p, 262144) || pcap_set_promisc(p, 0) ||
        pcap_set_timeout(p, 100) || pcap_set_buffer_size(p, (int)requested)) return 7;
    // Any activation warning is a failed preflight, not silently accepted.
    if (pcap_activate(p) != 0) return 8;
    unsigned effective = 0;
    int fd = pcap_get_selectable_fd(p);
#ifdef __APPLE__
    if (fd < 0 || fd >= FD_SETSIZE || ioctl(fd, BIOCGBLEN, &effective) || !effective ||
        effective > 32U*1024*1024) return 9;
#else
    // Linux fixture compilation does not substitute for actual Darwin allocation.
    (void)effective;
    return 9;
#endif
    struct bpf_program program;
    if (pcap_compile(p, &program, filter, 1, PCAP_NETMASK_UNKNOWN)) return 10;
    int rc = pcap_setfilter(p, &program);
    pcap_freecode(&program);
    if (rc || pcap_setnonblock(p, 1, err)) return 11;
    FILE *raw = fresh(argv[5]);
    if (!raw) return 12;
    if (setvbuf(raw, NULL, _IOFBF, 4*1024*1024)) return 13;
    dump = pcap_dump_fopen(p, raw);
    if (!dump) return 14;
    FILE *samples = metadata(argv[6], "samples.jsonl");
    if (!samples) return 15;
    struct sigaction action;
    memset(&action, 0, sizeof action);
    action.sa_handler = stop;
    sigemptyset(&action.sa_mask);
    if (sigaction(SIGINT, &action, NULL) || sigaction(SIGTERM, &action, NULL)) return 16;
    struct pcap_stat stats;
    if (pcap_stats(p, &stats)) return 17;
    FILE *ready = metadata(argv[6], "ready.partial");
    if (!ready) return 18;
    fprintf(ready, "{\"requested_buffer_bytes\":%u,\"effective_buffer_bytes\":%u,"
            "\"snaplen\":%d,\"datalink\":%d}\n", requested, effective,
            pcap_snapshot(p), pcap_datalink(p));
    if (fclose(ready)) return 19;
    char ready_partial[4096], ready_path[4096];
    if (snprintf(ready_partial, sizeof ready_partial, "%s/ready.partial", argv[6]) >= (int)sizeof ready_partial ||
        snprintf(ready_path, sizeof ready_path, "%s/ready.json", argv[6]) >= (int)sizeof ready_path ||
        link(ready_partial, ready_path) || unlink(ready_partial)) return 19;
    double start = now(), last_sample = start, last_packet = start, stopping = 0;
    double dispatch_seconds = 0, max_dispatch_seconds = 0;
    uint64_t last_bytes = 24;
    int drained = 0, previous_batch = 0;
    while (!failed) {
        double t = now();
        if (stop_requested && !stopping) { stopping = t; last_packet = t; }
        if (!stopping && t - start > seconds) { failed = 22; break; }
        if (stopping && t - stopping > 5) { failed = 23; break; }
        fd_set reads;
        FD_ZERO(&reads); FD_SET(fd, &reads);
        struct timeval wait = {0, 100000};
        int selected = previous_batch ? 0 : select(fd + 1, &reads, NULL, NULL, &wait);
        if (selected < 0 && errno != EINTR) { failed = 24; break; }
        // Nonblocking dispatch also drains a partially consumed libpcap batch.
        double before = now();
        int n = pcap_dispatch(p, 4096, packet, NULL);
        double duration = now() - before;
        dispatch_seconds += duration;
        if (duration > max_dispatch_seconds) max_dispatch_seconds = duration;
        if (n < 0) { failed = 25; break; }
        previous_batch = n;
        if (n) last_packet = now();
        if (ferror(raw)) { failed = 26; break; }
        t = now();
        if (t - last_sample >= 1) {
            struct statvfs space;
            struct rusage usage;
            if (statvfs(argv[6], &space) || getrusage(RUSAGE_SELF, &usage) || pcap_stats(p, &stats)) {
                failed = 27; break;
            }
            uint64_t free_bytes = (uint64_t)space.f_bavail * space.f_frsize;
            fprintf(samples, "{\"elapsed_seconds\":%.6f,\"packets\":%llu,\"pcap_bytes\":%llu,"
                    "\"interval_bytes_per_second\":%.3f,\"received\":%u,\"dropped\":%u,"
                    "\"interface_dropped\":%u,\"free_bytes\":%llu,\"maxrss_native\":%ld,"
                    "\"user_seconds\":%.6f,\"system_seconds\":%.6f}\n",
                    t-start, (unsigned long long)packets, (unsigned long long)file_bytes,
                    (file_bytes-last_bytes)/(t-last_sample), stats.ps_recv, stats.ps_drop,
                    stats.ps_ifdrop, (unsigned long long)free_bytes, usage.ru_maxrss,
                    usage.ru_utime.tv_sec + usage.ru_utime.tv_usec/1e6,
                    usage.ru_stime.tv_sec + usage.ru_stime.tv_usec/1e6);
            if (fflush(samples)) { failed = 28; break; }
            last_sample = t; last_bytes = file_bytes;
            if (free_bytes < min_free) { failed = 29; break; }
        }
        if (stopping && !n && t - last_packet >= 1) { drained = 1; break; }
    }
    double flush_start = now();
    if (pcap_dump_flush(dump) || fsync(fileno(raw))) failed = 30;
    // pcap_dump_close returns void. Explicit flush+fsync and ferror precede it.
    if (ferror(raw)) failed = 31;
    pcap_dump_close(dump);
    double flush_seconds = now()-flush_start;
    if (pcap_stats(p, &stats)) { memset(&stats, 0, sizeof stats); failed = 32; }
    pcap_close(p);
    if (fclose(samples)) failed = 33;
    int valid = !failed && drained && stop_requested && packets && stats.ps_recv &&
                !stats.ps_drop && !stats.ps_ifdrop && !truncated;
    FILE *summary = metadata(argv[6], "summary.json");
    if (!summary) return 34;
    fprintf(summary, "{\"capture_health_valid\":%s,\"completeness_proven\":false,\"error_code\":%d,\"drained\":%s,"
            "\"requested_buffer_bytes\":%u,\"effective_buffer_bytes\":%u,"
            "\"packets\":%llu,\"packet_bytes\":%llu,\"pcap_bytes\":%llu,"
            "\"truncated_packets\":%llu,\"received\":%u,\"dropped\":%u,"
            "\"interface_dropped\":%u,\"elapsed_seconds\":%.6f,"
            "\"dispatch_seconds\":%.6f,\"max_dispatch_seconds\":%.6f,\"flush_seconds\":%.6f}\n",
            valid ? "true":"false", failed, drained ? "true":"false", requested, effective,
            (unsigned long long)packets, (unsigned long long)packet_bytes, (unsigned long long)file_bytes,
            (unsigned long long)truncated, stats.ps_recv, stats.ps_drop, stats.ps_ifdrop,
            now()-start, dispatch_seconds, max_dispatch_seconds, flush_seconds);
    if (fclose(summary)) return 35;
    return valid ? 0 : 1;
}
