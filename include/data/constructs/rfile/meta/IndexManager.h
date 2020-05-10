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

#ifndef INDEXMANAGER_H_
#define INDEXMANAGER_H_

#include "data/streaming/OutputStream.h"
#include "data/streaming/input/InputStream.h"
#include "data/streaming/input/NetworkOrderInputStream.h"
#include "data/streaming/Streams.h"

#include "../../compressor/compressor.h"
#include "SerializedIndex.h"
#include "IndexEntry.h"
#include "IndexBlock.h"
#include "BlockLookup.h"
#include "Block.h"
#include "../../Key.h"
namespace cclient {
namespace data {

class IndexManager : public BlockLookup, public cclient::data::streams::StreamInterface {
 public:
  explicit IndexManager(std::unique_ptr<cclient::data::compression::Compressor> compressorRef, cclient::data::streams::InputStream *blockReader, int version);

  virtual ~IndexManager() {
  }
  uint64_t
  read(cclient::data::streams::InputStream *in);

  std::shared_ptr<SerializedIndex> lookup(const std::shared_ptr<Key> &key) {
    std::shared_ptr<Block> block = std::make_shared<Block>(this, indexBlock);
    std::shared_ptr<SerializedIndex> index = std::make_shared<SerializedIndex>(block, block->lookup(key));

    return index;
  }

  int getSize() {
    return size;
  }

  std::shared_ptr<IndexBlock> getIndexBlock(const std::shared_ptr<IndexEntry> &ie) {

    blockReader->seek(ie->getOffset());

    uint8_t *compressedValue = new uint8_t[ie->getCompressedSize()];

    blockReader->readBytes(compressedValue, ie->getCompressedSize());

    compressorRef->setInput((const char*) compressedValue, 0, ie->getCompressedSize());

    cclient::data::streams::ByteOutputStream outStream(ie->getRawSize());

    compressorRef->decompress(&outStream);

    cclient::data::streams::EndianInputStream returnStream(outStream.getByteArray(), outStream.getSize(), true);

    std::shared_ptr<IndexBlock> block = std::make_shared<IndexBlock>(version);
    block->read(&returnStream);

    return block;

  }
 protected:
  int size;
  int version;
  cclient::data::streams::InputStream *blockReader;
  std::unique_ptr<cclient::data::compression::Compressor> compressorRef;
  std::shared_ptr<IndexBlock> indexBlock;

};

}
}

#endif
