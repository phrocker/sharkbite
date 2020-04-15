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
#include <stdexcept>
#include <cstring>
#include <vector>

#include "data/constructs/compressor/../../streaming/OutputStream.h"
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/compressor/zlibCompressor.h"
#include "data/extern/fastmemcpy/FastMemCpy.h"

typedef std::vector<char> buffer_t;

namespace cclient {
namespace data {
namespace compression {
/**
 * Compression method.
 * @param out_stream.
 */
void ZLibCompressor::compress(cclient::data::streams::OutputStream *out_stream) {
  if (!init)
    throw std::runtime_error("Failure during compression; compression not initialized");

  if (len == 0)
    return;

  // variable used for the return code.
  int r = 0;

  c_stream.zalloc = (alloc_func) 0;
  c_stream.zfree = (free_func) 0;
  c_stream.opaque = (voidpf) 0;

  r = deflateInit(&c_stream, 6);
  if (r != Z_OK)
    throw std::runtime_error("Failure initializing compression");

  rawSize += len;
  // estimate the output buffer.
  output_length = len + len / 1000 + 12 + 1;

  if (output_length > out_buf.size())
    out_buf.resize(output_length);
  //out_buf = new Bytef[output_length];

  Bytef *casted_buffer = (Bytef*) ((char*) (buffer + off));

  c_stream.next_in = casted_buffer;
  c_stream.next_out = out_buf.data();
  c_stream.avail_in = len;
  c_stream.avail_out = output_length;
  c_stream.total_in = 0;
  c_stream.total_out = 0;
  total_out = 0;

  /*
   * Attempt to go through all input data. we know we have enough
   * length in the out_buf to compress the data, so we will go through
   * as many attempts at compression as the compressor will allow.
   */
  while (c_stream.total_in < len && c_stream.total_out < output_length) {

    r = deflate(&c_stream, Z_NO_FLUSH);
    if (r != Z_OK)
      throw std::runtime_error("Failure during compression; r != Z_OK");
  }
  // finish compression.
  r = deflate(&c_stream, Z_FINISH);

  if (r == Z_STREAM_END) {
    // if we have successful compression, write the data
    // to the output stream. and increment total_out.
    out_stream->write((const char*) out_buf.data(), c_stream.total_out);

    total_out += c_stream.total_out;
  } else {
    throw std::runtime_error("Failure during compression; r != Z_STREAM_END");
  }

  len = 0;
  // delete the output buffer and end compression.

  deflateEnd(&c_stream);
  len = 0;

}

#define CHECK_ERR(err, msg) { \
    if (err != Z_OK) { \
        throw std::runtime_error ("Failure during decompression"); \
    } \
}

/**
 * Deompression method.
 * @param out_stream.
 */
void ZLibCompressor::decompress(cclient::data::streams::ByteOutputStream *out_stream, char *in_buf, size_t size) {
  if (!init)
    throw std::runtime_error("Failure during compression; compression not initialized");

  auto my_len = size == 0 ? len : size;
  char *ptr = in_buf == nullptr ? (buffer + off) : in_buf;

  if (my_len == 0)
    return;

  // variable used for the return code.
  int r = 0;

  c_stream.zalloc = (alloc_func) 0;
  c_stream.zfree = (free_func) 0;
  c_stream.opaque = (voidpf) 0;

  r = inflateInit(&c_stream);
  if (r != Z_OK)
    throw std::runtime_error("Failure initializing compression");

  rawSize += my_len;
  // estimate the output buffer.

  output_length = my_len + my_len / 1000 + 12 + 1;
  //if (output_length > out_buf.size())
//    out_buf.resize(output_length*2);
  out_stream->flush();
  out_stream->ensure(output_length);
  Bytef *casted_buffer = (Bytef*) (ptr);

  c_stream.next_in = casted_buffer;
  c_stream.next_out = (Bytef*)out_stream->getByteArrayAtPosition();    //out_buf.data();
  c_stream.avail_in = my_len;
  c_stream.avail_out = output_length;
  c_stream.total_in = 0;
  c_stream.total_out = 0;
  total_out = 0;
  int ret = 0;
  int err = 0;
  int have = 0;
  for (uint32_t i = 0; i < my_len; i += output_length) {
    c_stream.avail_in = my_len - i;
    c_stream.next_in = casted_buffer + i;

    do {
      have = 0;

      c_stream.avail_out = output_length;       // H
      c_stream.next_out = (Bytef*)out_stream->getByteArrayAtPosition();

      ret = inflate(&c_stream, Z_NO_FLUSH);  // I

      if (ret == Z_STREAM_ERROR) {
        throw std::runtime_error("Zstream error. Invalid decompressor");
      }
      switch (ret) {
        case Z_NEED_DICT:
          ret = Z_DATA_ERROR; /* and fall through */
        case Z_DATA_ERROR:
        case Z_MEM_ERROR:

          (void) inflateEnd(&c_stream);
          CHECK_ERR(err, "Z_MEM_ERROR")
          ;
      }

      have = output_length - c_stream.avail_out;     // J

      out_stream->ensure(output_length, have);
//
      //    out_stream->write((const char*) out_buf.data(), have);

    } while (c_stream.avail_out == 0);         // K
  }
  total_out += c_stream.total_out;
  err = inflateEnd(&c_stream);

  len = 0;

}

}

}
}
