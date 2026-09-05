#include "../collector_darwin.h"
#include <inttypes.h>
#include <stdio.h>

int main(void) {
	mach_port_t host = mach_host_self();
	mach_port_urefs_t before, after;
	if(mach_port_get_refs(mach_task_self(), host, MACH_PORT_RIGHT_SEND, &before) != KERN_SUCCESS) return 1;
	uint64_t total, idle, rx, tx, signature;
	double memory;
	for(int index = 0; index < 1000; index++) {
		if(bsbctl_cpu_counters(&total, &idle) != 0 || bsbctl_memory_percent(&memory) != 0) return 2;
	}
	if(mach_port_get_refs(mach_task_self(), host, MACH_PORT_RIGHT_SEND, &after) != KERN_SUCCESS) return 3;
	mach_port_deallocate(mach_task_self(), host);
	if(before != after) {
		fprintf(stderr, "host send rights grew: %u -> %u\n", before, after);
		return 4;
	}
	struct if_msghdr2 fixture[3] = {0};
	for(int index = 0; index < 3; index++) {
		fixture[index].ifm_msglen = sizeof(fixture[index]);
		fixture[index].ifm_version = RTM_VERSION;
		fixture[index].ifm_type = RTM_IFINFO2;
		fixture[index].ifm_index = index + 1;
		fixture[index].ifm_data.ifi_ibytes = (1ULL << 40) + index;
		fixture[index].ifm_data.ifi_obytes = (1ULL << 42) + index;
	}
	fixture[0].ifm_flags = IFF_UP;
	fixture[1].ifm_flags = IFF_UP | IFF_LOOPBACK;
	/* The third interface is down; neither it nor loopback contributes. */
	if(bsbctl_parse_network_counters((const unsigned char*)fixture, sizeof(fixture), &rx, &tx, &signature) != 0) return 5;
	if(rx != (1ULL << 40) || tx != (1ULL << 42) || signature == 0) {
		fprintf(stderr, "64-bit network totals: rx=%" PRIu64 ", tx=%" PRIu64 "\n", rx, tx);
		return 6;
	}
	uint64_t previous_signature = signature;
	fixture[0].ifm_index++;
	if(bsbctl_parse_network_counters((const unsigned char*)fixture, sizeof(fixture), &rx, &tx, &signature) != 0 || signature == previous_signature) return 7;
	fixture[0].ifm_msglen = 0;
	if(bsbctl_parse_network_counters((const unsigned char*)fixture, sizeof(fixture), &rx, &tx, &signature) == 0) return 8;
	fixture[0].ifm_msglen = sizeof(fixture[0]);
	if(bsbctl_parse_network_counters((const unsigned char*)fixture, sizeof(fixture[0]) - 1, &rx, &tx, &signature) == 0) return 9;
	if(bsbctl_network_counters(&rx, &tx, &signature) != 0) return 10;
	printf("1000 native samples: host send rights %u -> %u; 64-bit ABI and framing checks passed\n", before, after);
	return 0;
}
