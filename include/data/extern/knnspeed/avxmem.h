//==================================================================================================================================
//  AVX Memory Functions: Main Header
//==================================================================================================================================
//
// Version 1.3875
//
// Author:
//  KNNSpeed
//
// Source Code:
//  https://github.com/KNNSpeed/AVX-Memmove
//
// Minimum requirement:
//  x86_64 CPU with SSE4.2, but AVX2 or later is *highly* recommended
//
// This file provides function prototypes for AVX_memmove, AVX_memcpy, AVX_memset, and AVX_memcmp
//
// NOTE: If you need to move/copy memory between overlapping regions, use AVX_memmove instead of AVX_memcpy.
// AVX_memcpy does contain a redirect to AVX_memmove if an overlapping region is found, but it is disabled by default
// since it adds extra latency that really can be avoided by using AVX_memmove directly.
//

#ifndef _avxmem_H
#define _avxmem_H

#include <stddef.h>
#include <stdint.h>
#ifdef NATIVE_ARCH
#include <immintrin.h>
#endif
// Size limit (in bytes) before switching to non-temporal/streaming loads & stores
// Applies to: AVX_memmove, AVX_memset, and AVX_memcpy
#define CACHESIZELIMIT 3*1024*1024 // 3 MB

//-----------------------------------------------------------------------------
// Main Functions:
//-----------------------------------------------------------------------------

// Calling the individual subfunctions directly is also OK. That's why this header is so huge!
int AVX_memcmp(const void *str1, const void *str2, size_t numbytes, int equality);

#ifdef NATIVE_ARCH
//-----------------------------------------------------------------------------
// MEMCMP:
//-----------------------------------------------------------------------------

int memcmp_large(const void *str1, const void *str2, size_t numbytes);
int memcmp_large_eq(const void *str1, const void *str2, size_t numbytes);

int memcmp_large_a(const void *str1, const void *str2, size_t numbytes);
int memcmp_large_eq_a(const void *str1, const void *str2, size_t numbytes);

// Scalar
int memcmp (const void *str1, const void *str2, size_t count);
int memcmp_eq (const void *str1, const void *str2, size_t count);
int memcmp_16bit(const void *str1, const void *str2, size_t count);
int memcmp_16bit_eq(const void *str1, const void *str2, size_t count);
int memcmp_32bit(const void *str1, const void *str2, size_t count);
int memcmp_32bit_eq(const void *str1, const void *str2, size_t count);
int memcmp_64bit(const void *str1, const void *str2, size_t count);
int memcmp_64bit_eq(const void *str1, const void *str2, size_t count);

// SSE4.2 (Unaligned)

int memcmp_128bit_u(const void *str1, const void *str2, size_t count);
int memcmp_128bit_eq_u(const void *str1, const void *str2, size_t count);

// SSE4.2 (Aligned)
int memcmp_128bit_a(const void *str1, const void *str2, size_t count);
int memcmp_128bit_eq_a(const void *str1, const void *str2, size_t count);

// AVX2
#ifdef __AVX2__
// Unaligned
int memcmp_256bit_u(const void *str1, const void *str2, size_t count);
int memcmp_256bit_eq_u(const void *str1, const void *str2, size_t count);

// Aligned
int memcmp_256bit_a(const void *str1, const void *str2, size_t count);
int memcmp_256bit_eq_a(const void *str1, const void *str2, size_t count);
#endif

// AVX512
#ifdef __AVX512F__
// Unaligned
int memcmp_512bit_u(const void *str1, const void *str2, size_t count);
int memcmp_512bit_eq_u(const void *str1, const void *str2, size_t count);

// Aligned
int memcmp_512bit_a(const void *str1, const void *str2, size_t count);
int memcmp_512bit_eq_a(const void *str1, const void *str2, size_t count);
#endif
// END MEMCMP
#endif
#endif /* _avxmem_H */
