// SPDX-License-Identifier: AGPL-3.0-only
// Read-only process identity; no arguments, paths or process memory exported.
#include <errno.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#ifdef __APPLE__
#include <libproc.h>
#include <sys/proc_info.h>
#endif

int main(int argc, char **argv) {
    if (argc != 2) return 2;
    char *end;
    errno = 0;
    long pid = strtol(argv[1], &end, 10);
    if (errno || !*argv[1] || *end || pid <= 0 || pid > INT_MAX) return 2;
#ifdef __APPLE__
    struct proc_bsdinfo info;
    char path[PROC_PIDPATHINFO_MAXSIZE] = {0};
    int n = proc_pidinfo((int)pid, PROC_PIDTBSDINFO, 0, &info, sizeof info);
    if (n != (int)sizeof info || proc_pidpath((int)pid, path, sizeof path) <= 0) {
        printf("{\"available\":false,\"error\":%d}\n", errno);
        return 0;
    }
    printf("{\"available\":true,\"pid\":%u,\"ppid\":%u,\"pgid\":%u,"
           "\"start_sec\":%llu,\"start_usec\":%llu,\"expected_binary\":%s}\n",
           info.pbi_pid, info.pbi_ppid, info.pbi_pgid,
           (unsigned long long)info.pbi_start_tvsec,
           (unsigned long long)info.pbi_start_tvusec,
           strcmp(path, "/usr/sbin/smbd") == 0 ? "true" : "false");
    return 0;
#else
    puts("{\"available\":false,\"unsupported_platform\":true}");
    return 0;
#endif
}
