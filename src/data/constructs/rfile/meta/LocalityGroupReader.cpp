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

#include "data/constructs/Key.h"
#include "data/constructs/rfile/meta/LocalityGroupReader.h"
#include <thread>
#include <chrono>
namespace cclient {
namespace data {

void LocalityGroupReader::seek(cclient::data::streams::StreamRelocation *position) {
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
    bool filtered = false;
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
        currentStream = std::move(getDataBlock(startBlock + iiter->getPreviousIndex()));
      } else {
        currentStream = std::move(getDataBlock(indexEntry->getOffset(), indexEntry->getCompressedSize(), indexEntry->getRawSize()));
      }
      checkRange = newSeekRequest->getRange()->afterEndKey(indexEntry->getKey());
      if (!checkRange)
        topExists = true;

      // don't concern ourselves with block indexing

      std::vector<char> valueArray;

      std::shared_ptr<Key> currKey = 0;

      SkippedRelativeKey skipRR(&allocatorInstance);
      skipRR.filterVisibility(auths);
      filtered = skipRR.skip(currentStream.get(), startKey, &valueArray, prevKey, currKey);

      if (skipRR.getPrevKey() != NULL) {
        prevKey = std::make_shared<Key>(skipRR.getPrevKey());
      } else
        prevKey = NULL;
      entriesLeft -= skipRR.getSkipped();
      val = std::make_shared<Value>();
      val->setValue((uint8_t*) valueArray.data(), valueArray.size(), 0);
      rKey = skipRR.getRelativeKey();
      /**
       * Visibility filtering when done at this level allows us to more efficiently skip index blocks.
       * Since we'll be doing a merged read it is safe to
       */
      if (!auths.empty()) {
        rKey->filterVisibility(auths);
      }

      if (filtered) {
        entriesSkipped++;
        rKey->setFiltered();
        // start readahead
        startReadAhead();
        next();
        if (!topExists) {
          return;
        }
      }
    }
  }

  topExists = (rKey != NULL && (currentRange->getInfiniteStopKey() || !currentRange->afterEndKey(getTopKey())));
  while (hasTop() && !currentRange->getInfiniteStartKey() && currentRange->beforeStartKey(getTopKey())) {
    next();
  }
  if (topExists && readAheadEnabled && iiter->hasNext()) {
    startReadAhead();
  } else {
    readAheadResult.checkRange = false;
    readAheadResult.hasData = true;
  }
}

void LocalityGroupReader::startReadAhead() {
  if (readAheadRunning)
    return;
  readAhead = std::async(std::launch::async, [&] {
    readAheadResult.hasData = false;
    size_t count = 0;
    readAheadRunning = true;
    while(readAheadRunning) {
      if (iiter->hasNext()) {
        (*iiter)++;
        std::shared_ptr<IndexEntry> indexEntry = iiter->get();
        readAheadResult.entries = indexEntry->getNumEntries();

        if (version == 3 || version == 4) {
          readAheadResult.stream = std::move(getDataBlock(startBlock + iiter->getPreviousIndex()));
        } else {
          readAheadResult.stream = std::move(getDataBlock(indexEntry->getOffset(), indexEntry->getCompressedSize(), indexEntry->getRawSize(), false));
        }
        readAheadResult.checkRange = !currentRange->afterEndKey(indexEntry->getKey());
      }
      else {
        readAheadResult.checkRange = false;
      }
      readAheadResult.hasData=true;
      readAheadConsumerCondition.notify_one();
      if (!readAheadResult.checkRange) {
        break;
      }
      std::unique_lock<std::mutex> lock(readAheadMutex);
      readAheadCondition.wait(lock, [&] {
            return !readAheadRunning || readAheadResult.interrupt;
          });
      readAheadResult.interrupt = false;
      lock.unlock();

      count++;
    }

    return count;

  });
  while (!readAheadRunning) {
    // a long sleep isn't needed here and avoids starvation on
    // single CPU systems
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
}

void LocalityGroupReader::next() {
  if (!hasTop())
    throw std::runtime_error("Illegal State Exception");
  if (SH_UNLIKELY(entriesLeft == 0)) {
    currentStream->close();
    if (readAheadEnabled) {

      readAheadLabel:
      // data is available
      if (!readAheadResult.hasData) {
        std::unique_lock<std::mutex> lock(readAheadMutex);
        readAheadConsumerCondition.wait(lock, [&] {
          return !readAheadRunning || readAheadResult.hasData;
        });
        lock.unlock();
      }
      if (readAheadResult.hasData && readAheadResult.checkRange) {
        entriesLeft = readAheadResult.entries;
        currentStream = std::move(readAheadResult.stream);
        topExists = true;
        readAheadResult.hasData = false;
        readAheadResult.interrupt = true;
        readAheadCondition.notify_one();
      } else {
        rKey = 0;
        val = 0;
        readAheadResult.hasData = false;
        readAheadResult.interrupt = true;
        readAheadCondition.notify_one();
        topExists = false;
        return;
      }
    } else {
      if (iiter->hasNext()) {
        (*iiter)++;
        std::shared_ptr<IndexEntry> indexEntry = iiter->get();
        entriesLeft = indexEntry->getNumEntries();
        if (version == 3 || version == 4) {
          currentStream = getDataBlock(startBlock + iiter->getPreviousIndex());
        } else {
          currentStream = getDataBlock(indexEntry->getOffset(), indexEntry->getCompressedSize(), indexEntry->getRawSize());
        }
        checkRange = !currentRange->afterEndKey(indexEntry->getKey());
        if (!checkRange)
          topExists = true;
      } else {
        rKey = 0;
        val = 0;
        topExists = false;
        return;
      }
    }
  }

  do {
    prevKey = std::static_pointer_cast<Key>(rKey->getStream());
    rKey->read(currentStream.get());
    val->read(currentStream.get());
    entriesLeft--;
    if (!rKey->isFiltered()) {
      // if we aren't filtered we can exit the loop.
      break;
    }
    entriesSkipped++;
    if (entriesLeft == 0) {
      currentStream->close();
      // this handles the case where we have not started the read ahead
      // because we are just opening the RFile.
      if (SH_LIKELY(readAheadEnabled)) {
        goto readAheadLabel;
      }

      if (iiter->hasNext()) {
        (*iiter)++;
        std::shared_ptr<IndexEntry> indexEntry = iiter->get();
        entriesLeft = indexEntry->getNumEntries();
        if (version == 3 || version == 4) {
          currentStream = getDataBlock(startBlock + iiter->getPreviousIndex());
        } else {
          currentStream = getDataBlock(indexEntry->getOffset(), indexEntry->getCompressedSize(), indexEntry->getRawSize());
        }
        checkRange = !currentRange->afterEndKey(indexEntry->getKey());
        if (!checkRange)
          topExists = true;
      } else {
        rKey = 0;
        val = 0;
        topExists = false;
        return;
      }
      if (!hasTop())
        return;
    }
  } while (entriesLeft > 0);
  if (checkRange) {
    topExists = !currentRange->afterEndKey(getTopKey());
  }

}

std::unique_ptr<cclient::data::streams::InputStream> LocalityGroupReader::getDataBlock(uint32_t index) {

  BlockRegion *region = bcFile->getDataIndex()->getBlockRegion(index);
  region->setCompressor(bcFile->getDataIndex()->getCompressionAlgorithm().create());
  auto stream = region->readDataStream(reader);
  return stream;
}

std::unique_ptr<cclient::data::streams::InputStream> LocalityGroupReader::getDataBlock(uint64_t offset, uint64_t compressedSize, uint64_t rawSize, bool use_cached) {

  cclient::data::compression::Compressor *compressor;
  if (use_cached || !compressors.try_dequeue(compressor)) {
    compressor = bcFile->getDataIndex()->getCompressionAlgorithm().create(use_cached);
  }

  BlockRegion region(offset, compressedSize, rawSize, compressor);
  std::vector<uint8_t> *my_buf;
  cclient::data::streams::ByteOutputStream *bout;
  if (!compressedBuffers.try_dequeue(my_buf)) {
    my_buf = new std::vector<uint8_t>();
  }

  if (!outputBuffers.try_dequeue(bout)) {
    bout = new cclient::data::streams::ByteOutputStream(0);

  }

  auto stream = region.assimilateDataStream(reader, my_buf, bout, &outputBuffers);

  compressedBuffers.enqueue(my_buf);
  if (!use_cached)
    compressors.enqueue(compressor);

  return stream;
}

}

}
