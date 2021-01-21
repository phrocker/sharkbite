/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#ifndef ZLIB_COMPRESSOR_H
#define ZLIB_COMPRESSOR_H

#include <exception>
#include <vector>
#include <stdexcept>
#include "compressor.h"
#include "../../streaming/DataOutputStream.h"

#ifdef USE_LIB_DEFLATE
#include "common/common_defs.h"
#include "libdeflate.h"
#endif

#define GZ_NAME "gz"

namespace cclient
{
	namespace data
	{
		namespace compression
		{

			class ZLibCompressor : public Compressor
			{
			public:
				explicit ZLibCompressor();
				/**
				 * Sets the input length and intializes the compressor
				 * @param in_len input length
				 */
				explicit ZLibCompressor(uint32_t in_len);

				virtual ~ZLibCompressor();

				virtual std::unique_ptr<Compressor> newInstance()
				{
					return std::make_unique<ZLibCompressor>();
				}

				/**
				 * Returns the input length
				 * @returns input length
				 */
				uint32_t getBufferSize()
				{

					return input_length;
				}

				/**
				 * Returns the compression overhead. Please ZLib for more info
				 * @returns compression overhead
				 */
				uint32_t getCompressionOverHead()
				{
					return (input_length >> 4) + 64 + 3;
				}

				/**
				 * Returns the number of bytes written
				 * @returns raw, uncompressed size, of the data
				 *
				 */
				uint32_t bytesWritten()
				{
					return rawSize;
				}

				/**
				 * Returns the compressed size of the data
				 * @returns total_out
				 */
				uint32_t getCompressedSize()
				{
					return total_out;
				}

				/**
				 * Compression method.
				 * @param out_stream.
				 */
				void compress(cclient::data::streams::OutputStream *out_stream);

				/**
				 * Deompression method.
				 * @param out_stream.
				 */
				void decompress(cclient::data::streams::ByteOutputStream *out_stream, char *in_buf = nullptr, size_t size = 0);

			protected:

				/**
				 * Initializes the Zlib compressor, accepts the input
				 * length
				 * @param in_len input length
				 */
				void initialize(uint32_t in_len);

				std::string getName()
				{
					return GZ_NAME;
				}

				bool init;
				// z lib stream
				z_stream c_stream;
				// raw size of the uncompressed data.
				uint32_t rawSize;
				// input buffer.
				//std::vector<Bytef> in_buf;
				// total output size.
				uint32_t total_out;

				// output buffer.
				std::vector<Bytef> out_buf;
				// input length.
				uint32_t input_length;
				// output length
				uint32_t output_length;

				#ifdef USE_LIB_DEFLATE
					uint32_t load_u32_gzip(const char* p){
						return ((uint32_t)p[3] << 0) | ((uint32_t)p[2] << 8) |
								((uint32_t)p[1] << 16) | ((uint32_t)p[0] << 24);
					}

				struct libdeflate_decompressor *decompressor;
				#endif


			};
		} // namespace compression
	}	  // namespace data
} // namespace cclient
#endif
