// SPDX-License-Identifier: AGPL-3.0-only
// Independent, constant-memory, offline check for ONE ordinary IPv4 TCP flow.
// Conservatively rejects retransmission/reordering rather than guessing coverage.
// Not an SMB decoder, not a general Time Machine/PF coverage validator.
#include <pcap/pcap.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>

static uint16_t be16(const unsigned char *p) { return (uint16_t)(p[0]*256U+p[1]); }
static uint32_t be32(const unsigned char *p) {
    return (uint32_t)p[0]<<24 | (uint32_t)p[1]<<16 | (uint32_t)p[2]<<8 | p[3];
}
struct direction { uint32_t next, last_ack; uint64_t bytes; int syn, fin, ack_seen; };
int main(int argc, char **argv) {
    // PCAP PORT EXPECTED_C2S_BYTES EXPECTED_S2C_BYTES
    if (argc != 5) return 2;
    unsigned port = (unsigned)strtoul(argv[2], NULL, 10);
    uint64_t expected[2] = {strtoull(argv[3], NULL, 10), strtoull(argv[4], NULL, 10)};
    struct stat st;
    if (!port || port>65535 || stat(argv[1], &st) || st.st_size<24 || st.st_size>48LL*1024*1024*1024) return 2;
    char err[PCAP_ERRBUF_SIZE];
    pcap_t *p = pcap_open_offline(argv[1], err);
    if (!p) return 3;
    int link = pcap_datalink(p), bad = 0, result;
    uint64_t packets = 0;
    uint16_t client_port = 0;
    uint32_t client_ip = 0, server_ip = 0;
    struct direction d[2] = {{0},{0}};
    double first = 0, last = 0;
    struct pcap_pkthdr *h;
    const unsigned char *b;
    while ((result = pcap_next_ex(p, &h, &b)) == 1) {
        packets++;
        if (packets>10000000 || h->caplen != h->len || h->caplen>262144) { bad=1; break; }
        unsigned ip;
        if (link == DLT_NULL) {
            uint32_t family;
            if (h->caplen<4) { bad=1; break; }
            memcpy(&family, b, 4);
            if (family!=2 && be32(b)!=2) { bad=1; break; }
            ip=4;
        } else if (link == DLT_EN10MB) {
            if (h->caplen<14 || be16(b+12)!=0x0800) { bad=1; break; }
            ip=14;
        } else { bad=1; break; }
        if (h->caplen<ip+20 || b[ip]>>4!=4 || b[ip+9]!=6) { bad=1; break; }
        unsigned ihl=(b[ip]&15)*4, total=be16(b+ip+2);
        if (ihl<20 || total<ihl+20 || h->caplen<ip+total || be16(b+ip+6)&0x3fff) { bad=1; break; }
        const unsigned char *tcp=b+ip+ihl;
        unsigned thl=(tcp[12]>>4)*4;
        if (thl<20 || total<ihl+thl) { bad=1; break; }
        uint16_t src=be16(tcp), dst=be16(tcp+2);
        uint32_t sip=be32(b+ip+12), dip=be32(b+ip+16);
        unsigned flags=tcp[13], payload=total-ihl-thl;
        int direction;
        if (!client_port) {
            if (dst!=port || src==port || (flags&0x17)!=2 || payload) { bad=1; break; }
            client_port=src; client_ip=sip; server_ip=dip;
        }
        if (src==client_port && dst==port && sip==client_ip && dip==server_ip) direction=0;
        else if (src==port && dst==client_port && sip==server_ip && dip==client_ip) direction=1;
        else { bad=1; break; }
        struct direction *cur=&d[direction], *peer=&d[1-direction];
        uint32_t seq=be32(tcp+4), ack=be32(tcp+8);
        if (flags&4 || flags&0xc0) { bad=1; break; } // RST or unexpected ECN calibration state
        if (flags&2) {
            if (cur->syn || payload || flags&1 || (direction==1 && !(flags&16))) { bad=1; break; }
            cur->syn=1; cur->next=seq+1;
        } else {
            // Exact next sequence: missing, conflicting or ambiguous repeats fail closed.
            if (!cur->syn || seq!=cur->next || (cur->fin && (payload || flags&1))) { bad=1; break; }
            cur->next+=payload;
            cur->bytes+=payload;
            if (flags&1) { cur->next++; cur->fin=1; }
        }
        if (flags&16) {
            // ACK may lag, but never acknowledge unobserved bytes. Unwrap within <2^31.
            if (!peer->syn || (!cur->ack_seen && ack!=peer->next) || (int32_t)(ack-peer->next)>0 ||
                (cur->ack_seen && (int32_t)(ack-cur->last_ack)<0)) { bad=1; break; }
            cur->last_ack=ack; cur->ack_seen=1;
        } else if (!(flags&2) || direction) { bad=1; break; }
        if (direction==0 && payload) {
            double t=h->ts.tv_sec+h->ts.tv_usec/1e6;
            if (!first) first=t;
            if (t<last) { bad=1; break; }
            last=t;
        }
    }
    if (result!=-2) bad=1; // EOF only: truncated/error is never green.
    pcap_close(p);
    int complete=!bad && packets && d[0].syn && d[1].syn && d[0].fin && d[1].fin &&
        d[0].ack_seen && d[1].ack_seen && d[0].last_ack==d[1].next && d[1].last_ack==d[0].next &&
        d[0].bytes==expected[0] && d[1].bytes==expected[1];
    printf("{\"complete\":%s,\"decode_or_order_error\":%s,\"packets\":%llu,"
           "\"client_bytes\":%llu,\"server_bytes\":%llu,\"client_payload_span_seconds\":%.6f}\n",
           complete?"true":"false", bad?"true":"false", (unsigned long long)packets,
           (unsigned long long)d[0].bytes, (unsigned long long)d[1].bytes, first?last-first:0);
    return complete?0:1;
}
