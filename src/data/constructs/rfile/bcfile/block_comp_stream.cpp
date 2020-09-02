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
#include "data/constructs/rfile/bcfile/block_comp_stream.h"

#include <fstream>

#include "data/constructs/compressor/compressor.h"
#include "data/constructs/rfile/bcfile/../../../streaming/DataOutputStream.h"
#include "data/constructs/rfile/bcfile/../../../streaming/NetworkOrderStream.h"
#include "data/constructs/rfile/bcfile/../../../streaming/input/InputStream.h"
#include "data/constructs/rfile/bcfile/../../../streaming/input/NetworkOrderInputStream.h"
namespace cclient {
namespace data {

BlockCompressorStream::BlockCompressorStream(
    streams::OutputStream *out_stream,
    std::unique_ptr<cclient::data::compression::Compressor> compressor,
    BlockRegion *region)
    : BlockStreambuffer(compressor->getBufferSize()),
      cclient::data::streams::DataOutputStream(new streams::BigEndianOutStream(
          new OutputStream((std::ostream *)this, out_stream->getPos()))),
      compress(std::move(compressor)),
      output_stream(out_stream),
      std::ostream((BlockStreambuffer *)this),
      std::istream(this),
      std::ios(0),
      blockLoc(0),
      writeStart(false),
      associatedRegion(region) {}

BlockCompressorStream::BlockCompressorStream(
    InputStream *in_stream,
    std::unique_ptr<compression::Compressor> decompressor, BlockRegion *region)
    : BlockStreambuffer(decompressor->getBufferSize()),
      cclient::data::streams::DataOutputStream(NULL),
      cclient::data::streams::EndianInputStream(),
      std::istream(this),
      std::ios(0),
      blockLoc(0),
      writeStart(false),
      associatedRegion(region),
      output_stream(NULL),
      compress(std::move(decompressor)) {
  uint64_t prevPosition = in_stream->getPos();

  in_stream->seek(region->getOffset());

  uint8_t *compressedValue = new uint8_t[region->getCompressedSize()];

  in_stream->readBytes(compressedValue, region->getCompressedSize());

  compress->setInput((const char *)compressedValue, 0,
                     region->getCompressedSize());

  streams::ByteOutputStream *outStream =
      new streams::ByteOutputStream(region->getRawSize());

  compress->decompress(outStream);

  setArray(outStream->getByteArray(), outStream->getSize(), true);

  // reset the location

  in_stream->seek(prevPosition);

  delete[] compressedValue;

  delete outStream;
}

BlockCompressorStream::~BlockCompressorStream() {}

}  // namespace data
}  // namespace cclient
