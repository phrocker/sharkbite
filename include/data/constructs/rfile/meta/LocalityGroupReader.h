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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_

#include "data/streaming/accumulo/StreamSeekable.h"
#include "data/streaming/OutputStream.h"
#include "data/streaming/input/InputStream.h"
#include "data/streaming/Streams.h"
#include "data/streaming/StreamRelocation.h"
#include "data/streaming/StreamEnvironment.h"
#include "data/streaming/accumulo/FileIterator.h"
#include "data/streaming/DataStream.h"
#include "data/exceptions/IllegalArgumentException.h"
#include "data/exceptions/InterationInterruptedException.h"
#include "IndexEntry.h"
#include <memory>

#include "../bcfile/BlockCompressedFile.h"
#include "data/extern/concurrentqueue/concurrentqueue.h"
// constructs

#include "data/constructs/Key.h"
#include "data/constructs/value.h"
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/Range.h"
#include "data/constructs/SkippedRelativeKey.h"
#include "data/constructs/rkey.h"

namespace cclient {
namespace data {

class LocalityGroupReader : public cclient::data::streams::FileIterator {
 protected:
  BlockCompressedFile *bcFile;
  cclient::data::streams::InputStream *reader;
  int version;
  volatile bool closed;
  bool checkRange;
  volatile bool topExists = false;
  std::unique_ptr<cclient::data::streams::InputStream> currentStream;
  volatile bool interrupted = false;
  Range *currentRange;
  std::shared_ptr<SerializedIndex> iiter;
  std::shared_ptr<Key> prevKey;
  uint32_t entriesLeft;
  std::shared_ptr<IndexManager> index;

  moodycamel::ConcurrentQueue<std::vector<uint8_t>*> compressedBuffers;
  moodycamel::ConcurrentQueue<cclient::data::streams::ByteOutputStream*> outputBuffers;


  int blockCount;
  std::shared_ptr<Key> firstKey;
  int startBlock;

  std::shared_ptr<RelativeKey> rKey;

  std::shared_ptr<Value> val;

  void close() {
    if (NULL != currentStream) {
      currentStream->close();
      currentStream = NULL;
    }
    std::vector<uint8_t>* buf;
    while(compressedBuffers.size_approx() > 0){
      if ( compressedBuffers.try_dequeue( buf )) {
        delete buf;
      }
    }
  cclient::data::streams::ByteOutputStream  *stream;
    while(outputBuffers.size_approx() > 0){
      if ( outputBuffers.try_dequeue( stream )) {
        delete stream;
      }
    }
  }

 public:
  LocalityGroupReader(BlockCompressedFile *bcFile, cclient::data::streams::InputStream *input_stream, LocalityGroupMetaData *metadata, int version)
      :
      bcFile(bcFile),
      reader(input_stream),
      version(version),
      closed(false),
      checkRange(false),
      topExists(false),
      interrupted(false),
      currentRange(NULL),
      iiter(NULL),
      prevKey(
      NULL),
      entriesLeft(-1) {
    index = metadata->getIndexManager();
    firstKey = std::dynamic_pointer_cast<Key>(metadata->getFirstKey());
    startBlock = metadata->getStartBlock();
    blockCount = index->getSize();
    rKey = NULL;
  }

  virtual ~LocalityGroupReader() {
  }

  std::shared_ptr<Key> getFirstKey() {
    return firstKey;
  }

  std::shared_ptr<Key> getLastKey() {
    return iiter->get(iiter->size() - 1)->getKey();
  }

  std::shared_ptr<Key> getTopKey() {
    return rKey->getKey();
  }

std::shared_ptr<Value> getTopValue() {
    return val;
  }

  bool hasTop() {
    return topExists;
  }

  void seek(cclient::data::streams::StreamRelocation *position) {
    cclient::data::streams::StreamSeekable *newSeekRequest = dynamic_cast<cclient::data::streams::StreamSeekable*>(position);
    if (closed) {
      throw cclient::exceptions::IllegalArgumentException("Locality group reader closed");
    }

    if (newSeekRequest->getColumnFamilies()->size() > 0 || newSeekRequest->isInclusive()) {
      throw std::runtime_error("I do not know how to filter column families");
    }

    if (interrupted) {
      throw cclient::exceptions::IterationInterruptedException("interrupted");
    }

    currentRange = newSeekRequest->getRange();

    checkRange = true;

    if (blockCount == 0) {
      topExists = false;
      rKey = NULL;
      return;
    }

    std::shared_ptr<Key> startKey = NULL;
    if (NULL != currentRange->getStartKey()) {
      startKey = std::make_shared<Key>(currentRange->getStartKey());

    } else {
      startKey = std::make_shared<Key>();
    }

    bool reseek = true;

    if (currentRange->getStopKey() != NULL && firstKey > currentRange->getStopKey()) {
      // range is before the first key;
      reseek = false;
    }

    if (rKey != NULL) {
      // already done work
    } else {

    }

    if (reseek) {
      
      iiter = index->lookup(startKey);

      close();

      if (!iiter->isEnd()) {
        while (iiter->hasPrevious()) {
          std::shared_ptr<IndexEntry> ent = *(*iiter);
          if (*(iiter->getPrevious()) == *ent)
            iiter->previous();
          else
            break;
        }

        if (iiter->hasPrevious()) {
          prevKey = std::make_shared<Key>(iiter->getPrevious()->getKey());
        } else
          prevKey = std::make_shared<Key>();

        std::shared_ptr<IndexEntry> indexEntry = iiter->get();
        entriesLeft = indexEntry->getNumEntries();


        if (version == 3 || version == 4) {
          currentStream = getDataBlock(startBlock + iiter->getPreviousIndex());
        } else {
          currentStream = getDataBlock(indexEntry->getOffset(), indexEntry->getCompressedSize(),indexEntry->getRawSize());
        }
        checkRange = newSeekRequest->getRange()->afterEndKey(indexEntry->getKey());
        if (!checkRange)
          topExists = true;

        // don't concern outselves with block indexing

        std::vector<char> valueArray;

        std::shared_ptr<Key> currKey = 0;

        SkippedRelativeKey skipRR(currentStream.get(), startKey, &valueArray, prevKey, currKey);

        if (skipRR.getPrevKey() != NULL) {
          prevKey = std::make_shared<Key>(skipRR.getPrevKey());
        } else
          prevKey = NULL;
        entriesLeft -= skipRR.getSkipped();
        val = std::make_shared<Value>();
        val->setValue((uint8_t*) valueArray.data(), valueArray.size(), 0);
        rKey = skipRR.getRelativeKey();

      }
    }

    topExists = (rKey != NULL && (currentRange->getInfiniteStopKey() || !currentRange->afterEndKey(getTopKey())));
    while (hasTop() && !currentRange->getInfiniteStartKey() && currentRange->beforeStartKey(getTopKey())) {
      next();
    }
  }

  virtual inline void next() {

    if (!hasTop())
      throw std::runtime_error("Illegal State Exception");
    if (SH_UNLIKELY(entriesLeft == 0)) {
      currentStream->close();

      if (iiter->hasNext()) {
        (*iiter)++;
        std::shared_ptr<IndexEntry> indexEntry = iiter->get();
        entriesLeft = indexEntry->getNumEntries();
        if (version == 3 || version == 4) {
          currentStream = getDataBlock(startBlock + iiter->getPreviousIndex());
        } else {
          currentStream = getDataBlock(indexEntry->getOffset(),indexEntry->getCompressedSize(),indexEntry->getRawSize());
        }
        checkRange = !currentRange->afterEndKey(indexEntry->getKey());
        if (!checkRange)
          topExists = true;
      }
      else {
        rKey = 0;
        val = 0;
        topExists = false;
        return;
      }
    }
    prevKey = std::static_pointer_cast<Key>(rKey->getStream());
    rKey->read(currentStream.get());
    val->read(currentStream.get());
    entriesLeft--;
    if (checkRange){
      topExists = !currentRange->afterEndKey(getTopKey());
    }

  }

  std::unique_ptr<cclient::data::streams::InputStream>
  getDataBlock(uint32_t index) {

    BlockRegion *region = bcFile->getDataIndex()->getBlockRegion(index);
    region->setCompressor(bcFile->getDataIndex()->getCompressionAlgorithm().create());
    auto stream = region->readDataStream(reader);
    delete region;
    return stream;
  }

  std::unique_ptr<cclient::data::streams::InputStream>
  getDataBlock(uint64_t offset, uint64_t compressedSize, uint64_t rawSize) {


    cclient::data::compression::Compressor *compressor = bcFile->getDataIndex()->getCompressionAlgorithm().create();
    BlockRegion region(offset, compressedSize, rawSize, compressor);
    std::vector<uint8_t> *my_buf;
    cclient::data::streams::ByteOutputStream *bout;
    if (!compressedBuffers.try_dequeue(my_buf)){
        my_buf = new std::vector<uint8_t>();
    }
    else{
//      std::cout << "reusing buffer of size " << my_buf->size() << std::endl;
    }

    if (!outputBuffers.try_dequeue(bout)){
      bout = new cclient::data::streams::ByteOutputStream(0);
  
    }
    else{
  //    std::cout << "reusing buffer of size " << bout->getPos() << std::endl;
    }

    auto stream = region.assimilateDataStream(reader,my_buf,bout);

    compressedBuffers.enqueue(my_buf);
    outputBuffers.enqueue(bout);
    

    return stream;
  }

}

;
}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_ */

