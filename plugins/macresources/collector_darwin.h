#ifndef BSBCTL_COLLECTOR_DARWIN_H
#define BSBCTL_COLLECTOR_DARWIN_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <mach/host_info.h>
#include <net/if.h>
#include <net/route.h>
#include <sys/socket.h>
#include <sys/sysctl.h>

static int bsbctl_cpu_counters(uint64_t* total, uint64_t* idle) {
	host_cpu_load_info_data_t value;
	mach_msg_type_number_t count = HOST_CPU_LOAD_INFO_COUNT;
	mach_port_t host = mach_host_self();
	kern_return_t result = host_statistics(host, HOST_CPU_LOAD_INFO, (host_info_t)&value, &count);
	mach_port_deallocate(mach_task_self(), host);
	if(result != KERN_SUCCESS) return (int)result;
	*total = 0;
	for(int index = 0; index < CPU_STATE_MAX; index++) {
		*total += value.cpu_ticks[index];
	}
	*idle = value.cpu_ticks[CPU_STATE_IDLE];
	return 0;
}

static int bsbctl_memory_percent(double* percent) {
	vm_statistics64_data_t value;
	mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
	vm_size_t page_size = 0;
	mach_port_t host = mach_host_self();
	kern_return_t result = host_statistics64(host, HOST_VM_INFO64, (host_info64_t)&value, &count);
	if(result == KERN_SUCCESS) result = host_page_size(host, &page_size);
	mach_port_deallocate(mach_task_self(), host);
	if(result != KERN_SUCCESS) return (int)result;
	uint64_t physical = 0;
	size_t physical_size = sizeof(physical);
	if(sysctlbyname("hw.memsize", &physical, &physical_size, NULL, 0) != 0 || physical == 0) {
		return -1;
	}
	uint64_t pages = (uint64_t)value.active_count + (uint64_t)value.wire_count +
		(uint64_t)value.compressor_page_count;
	*percent = ((double)pages * (double)page_size / (double)physical) * 100.0;
	return 0;
}

/* NET_RT_IFLIST2 carries if_data64 inside if_msghdr2. getifaddrs instead
 * exposes the different, narrow if_data layout and cannot provide these totals. */
static int bsbctl_parse_network_counters(const unsigned char* bytes, size_t size,
	uint64_t* received, uint64_t* sent, uint64_t* signature) {
	*received = 0;
	*sent = 0;
	*signature = 0;
	uint64_t identities_sum = 0;
	uint64_t identities_xor = 0;
	uint64_t interfaces_count = 0;
	while(size != 0) {
		if(size < 4) return -1;
		uint16_t length;
		memcpy(&length, bytes, sizeof(length));
		if(length < 4 || length > size || bytes[2] != RTM_VERSION) return -1;
		if(bytes[3] == RTM_IFINFO2) {
			if(length < sizeof(struct if_msghdr2)) return -1;
			struct if_msghdr2 info;
			memcpy(&info, bytes, sizeof(info));
			if((info.ifm_flags & IFF_UP) != 0 && (info.ifm_flags & IFF_LOOPBACK) == 0) {
				*received += info.ifm_data.ifi_ibytes;
				*sent += info.ifm_data.ifi_obytes;
				uint64_t identity = ((uint64_t)info.ifm_index + 1) * 0x517cc1b727220a95ULL;
				identities_sum += identity;
				identities_xor ^= identity;
				interfaces_count++;
			}
		}
		bytes += length;
		size -= length;
	}
	*signature = identities_sum + (identities_xor * 0x517cc1b727220a95ULL) +
		(interfaces_count * 0x9e3779b97f4a7c15ULL);
	return 0;
}

static int bsbctl_network_counters(uint64_t* received, uint64_t* sent, uint64_t* signature) {
	int mib[] = {CTL_NET, PF_ROUTE, 0, 0, NET_RT_IFLIST2, 0};
	size_t size = 0;
	if(sysctl(mib, 6, NULL, &size, NULL, 0) != 0 || size > (1U << 20)) return -1;
	if(size == 0) return bsbctl_parse_network_counters(NULL, 0, received, sent, signature);
	unsigned char* bytes = malloc(size);
	if(bytes == NULL) return -1;
	int result = sysctl(mib, 6, bytes, &size, NULL, 0);
	if(result == 0) result = bsbctl_parse_network_counters(bytes, size, received, sent, signature);
	free(bytes);
	return result;
}

#endif
