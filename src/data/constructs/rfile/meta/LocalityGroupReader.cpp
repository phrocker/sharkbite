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

#include "data/constructs/rfile/meta/LocalityGroupReader.h"

#include <chrono>
#include <thread>

#include "data/constructs/Key.h"
namespace cclient {
namespace data {

std::vector<std::shared_ptr<cclient::data::Key>>
LocalityGroupReader::getBlockKeys(
    cclient::data::streams::StreamRelocation *position) {
  if (closed) {
    throw cclient::exceptions::IllegalArgumentException(
        "Locality group reader closed");
  }

  std::vector<std::shared_ptr<cclient::data::Key>> topKeys;

  auto seekRange = position->getRange();

  std::shared_ptr<Key> startKey = NULL;
  if (NULL != seekRange->getStartKey()) {
    startKey = std::make_shared<Key>(seekRange->getStartKey());

  } else {
    startKey = std::make_shared<Key>();
  }

  iiter = index->lookup(startKey);

  close();

  do {
    if (!iiter->isEnd()) {
      std::shared_ptr<IndexEntry> indexEntry = iiter->get();
      // place the key for the index entry into the array
      topKeys.emplace_back(indexEntry->getKey());
    }
    if (iiter->hasNext()) {
      (*iiter)++;
    } else {
      break;
    }
  } while (true);

  return topKeys;
}

void LocalityGroupReader::seek(
    cclient::data::streams::StreamRelocation *position) {
  if (closed) {
    throw cclient::exceptions::IllegalArgumentException(
        "Locality group reader closed");
  }

  if (position->getColumnFamilies()->size() > 0 || position->isInclusive()) {
    throw std::runtime_error("I do not know how to filter column families");
  }

  if (interrupted) {
    throw cclient::exceptions::IterationInterruptedException("interrupted");
  }

  currentRange = position->getRange();

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

  if (currentRange->getStopKey() != NULL &&
      currentRange->afterEndKey(firstKey)) {
    // range is before the first key;
    logging::LOG_TRACE(logger) << "reseek not needed for " << firstKey << " < "
                               << currentRange->getStopKey();
    close();
    reseek = false;
    iiter = nullptr;
    rKey = nullptr;
  }

  if (reseek) {
    iiter = index->lookup(startKey);

    close();

    if (!iiter) {
      return;
    }
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

      logging::LOG_TRACE(logger)
          << "Index Entry is " << indexEntry->getKey() << " on " << startKey;

      if (version == 3 || version == 4) {
        currentStream =
            std::move(getDataBlock(startBlock + iiter->getPreviousIndex()));
      } else {
        currentStream = std::move(getDataBlock(indexEntry->getOffset(),
                                               indexEntry->getCompressedSize(),
                                               indexEntry->getRawSize()));
      }
      checkRange = position->getRange()->afterEndKey(indexEntry->getKey());
      if (!checkRange) topExists = true;

      // don't concern ourselves with block indexing

      std::vector<char> valueArray;

      std::shared_ptr<Key> currKey = 0;

      SkippedRelativeKey skipRR(allocatorInstance);
      skipRR.filterVisibility(auths);
      skipRR.setAgeOff(ageoff_evaluator);
      skipRR.setKeyPredicate(key_predicate);
      filtered = skipRR.skip(currentStream.get(), startKey, &valueArray,
                             prevKey, currKey, entriesLeft);

      if (skipRR.getPrevKey() != NULL) {
        prevKey = std::make_shared<Key>(skipRR.getPrevKey());
      } else
        prevKey = NULL;
      logging::LOG_TRACE(logger) << "SRK skipped " << skipRR.getSkipped()
                                 << " keys of " << entriesLeft;
      uint32_t skp = skipRR.getSkipped();
      if (skp > entriesLeft) {
        entriesLeft = 0;
      } else
        entriesLeft -= skp;
      if (skp > 1) entriesSkipped += skp - 1;
      val = std::make_shared<Value>();
      val->setValue((uint8_t *)valueArray.data(), valueArray.size(), 0);
      rKey = skipRR.getRelativeKey();
      if (!rKey) {
        return;
      }
      /**
       * Visibility filtering when done at this level allows us to more
       * efficiently skip index blocks. Since we'll be doing a merged read it is
       * safe to
       */
      if (!auths.empty()) {
        rKey->filterVisibility(auths);
      }
      if (ageoff_evaluator) {
        rKey->setAgeOffEvaluator(ageoff_evaluator);
      }

      if (key_predicate) {
        rKey->setKeyPredicate(key_predicate);
      }

      if (filtered) {
        entriesSkipped++;
        rKey->setFiltered();
        // start readahead
        startReadAhead();
        next(false);
        if (!topExists) {
          return;
        }
      }
    } else {
      logging::LOG_TRACE(logger) << "reseek not necessary";
    }
  }

  topExists = rKey != NULL && !currentRange->afterEndKey(getTopKey());

  if (topExists && readAheadEnabled && (nullptr != iiter && iiter->hasNext())) {
    logging::LOG_TRACE(logger) << " Starting read ahead on " << getTopKey();
    startReadAhead();
  }

  while (hasTop() && !currentRange->getInfiniteStartKey() &&
         currentRange->beforeStartKey(getTopKey())) {
    next();
  }

  if (!(topExists && readAheadEnabled &&
        (nullptr != iiter && iiter->hasNext()))) {
    readAheadResult.checkRange = false;
    readAheadResult.hasData = true;
  }
}

void LocalityGroupReader::startReadAhead() {
  if (readAheadRunning) return;
  readAhead = std::async(std::launch::async, [&] {
    readAheadResult.hasData = false;
    readAheadResult.finished = false;
    size_t count = 0;
    readAheadRunning = true;
    while (readAheadRunning) {
      if (iiter->hasNext()) {
        (*iiter)++;
        std::shared_ptr<IndexEntry> indexEntry = iiter->get();
        readAheadResult.entries = indexEntry->getNumEntries();

        if (version == 3 || version == 4) {
          readAheadResult.stream =
              std::move(getDataBlock(startBlock + iiter->getPreviousIndex()));
        } else {
          readAheadResult.stream = std::move(getDataBlock(
              indexEntry->getOffset(), indexEntry->getCompressedSize(),
              indexEntry->getRawSize(), false));
        }
        readAheadResult.checkRange =
            currentRange->afterEndKey(indexEntry->getKey());
      } else {
        readAheadResult.checkRange = false;
        std::unique_lock<std::mutex> lock(readAheadMutex);
        readAheadResult.hasData = false;
        readAheadResult.finished = true;
        readAheadResult.entries = 0;
        readAheadConsumerCondition.notify_one();
        lock.unlock();
        break;
      }
      std::unique_lock<std::mutex> lock(readAheadMutex);
      readAheadResult.hasData = true;
      readAheadResult.finished = false;
      readAheadConsumerCondition.notify_one();
      if (readAheadResult.finished) {
        lock.unlock();
        break;
      }

      readAheadCondition.wait(
          lock, [&] { return !readAheadRunning || readAheadResult.interrupt; });
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

void LocalityGroupReader::next(bool errorOnNext) {
  if (errorOnNext && !hasTop())
    throw std::runtime_error("Illegal State Exception");
  if (SH_UNLIKELY(entriesLeft == 0)) {
    currentStream->close();
    if (readAheadEnabled) {
    readAheadLabel:
      // data is available
      if (!readAheadResult.hasData && !readAheadResult.finished) {
        std::unique_lock<std::mutex> lock(readAheadMutex);
        if (!readAheadResult.hasData && !readAheadResult.finished) {
          readAheadConsumerCondition.wait(lock, [&] {
            return !readAheadRunning || readAheadResult.hasData ||
                   readAheadResult.finished;
          });
          lock.unlock();
        }
      }
      if (readAheadResult.hasData) {
        entriesLeft = readAheadResult.entries;
        currentStream = std::move(readAheadResult.stream);
        if (!readAheadResult.checkRange)
          topExists = true;
        else {
          checkRange = true;
        }
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
          currentStream = getDataBlock(indexEntry->getOffset(),
                                       indexEntry->getCompressedSize(),
                                       indexEntry->getRawSize());
        }
        checkRange = !currentRange->afterEndKey(indexEntry->getKey());
        if (!checkRange) topExists = true;
      } else {
        rKey = 0;
        val = 0;
        topExists = false;
        return;
      }
    }
  }

  do {
    if (entriesLeft == 0) {
      topExists = false;
      return;
    }
    if (rKey != nullptr)
      prevKey = std::static_pointer_cast<Key>(rKey->getStream());
    else {
      rKey = std::make_shared<cclient::data::RelativeKey>(allocatorInstance);
    }
    if (auths.empty() && !key_predicate)
      rKey->read(currentStream.get());
    else
      rKey->readFiltered(currentStream.get());
    val = std::make_shared<Value>();
    val->read(currentStream.get());
    entriesLeft--;
    if (!rKey->isFiltered()) {
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
          currentStream = getDataBlock(indexEntry->getOffset(),
                                       indexEntry->getCompressedSize(),
                                       indexEntry->getRawSize());
        }
        checkRange = !currentRange->afterEndKey(indexEntry->getKey());
        if (!checkRange) topExists = true;
      } else {
        rKey = 0;
        val = 0;
        topExists = false;
        return;
      }
      if (!hasTop()) return;
    }
  } while (entriesLeft > 0);
  if (checkRange) {
    topExists = !currentRange->afterEndKey(getTopKey());
  }
}

std::unique_ptr<cclient::data::streams::InputStream>
LocalityGroupReader::getDataBlock(uint32_t index) {
  BlockRegion *region = bcFile->getDataIndex()->getBlockRegion(index);
  region->setCompressor(cclient::data::compression::CompressorFactory::create(
      bcFile->getDataIndex()->getCompressionAlgorithm()));
  auto stream = region->readDataStream(reader);
  return stream;
}

std::unique_ptr<cclient::data::streams::InputStream>
LocalityGroupReader::getDataBlock(uint64_t offset, uint64_t compressedSize,
                                  uint64_t rawSize, bool use_cached) {
  std::unique_ptr<cclient::data::compression::Compressor> compressor =
      cclient::data::compression::CompressorFactory::create(
          bcFile->getDataIndex()->getCompressionAlgorithm());

  BlockRegion region(offset, compressedSize, rawSize, std::move(compressor));
  std::vector<uint8_t> *my_buf;
  cclient::data::streams::ByteOutputStream *bout;
  if (!compressedBuffers.try_dequeue(my_buf)) {
    my_buf = new std::vector<uint8_t>();
  }

  if (!outputBuffers.try_dequeue(bout)) {
    bout = new cclient::data::streams::ByteOutputStream(0);
  }

  auto stream =
      region.assimilateDataStream(reader, my_buf, bout, &outputBuffers);

  compressedBuffers.enqueue(my_buf);

  return stream;
}

void LocalityGroupReader::close() {
  if (readAheadEnabled) {
    if (readAheadRunning) {
      readAheadRunning = false;
      readAheadResult.interrupt = true;
      readAheadCondition.notify_one();
      readAhead.wait();
    }
  }

  if (NULL != currentStream) {
    currentStream->close();
    currentStream = NULL;
  }
  std::vector<uint8_t> *buf;
  while (compressedBuffers.size_approx() > 0) {
    if (compressedBuffers.try_dequeue(buf)) {
      delete buf;
    }
  }
  cclient::data::streams::ByteOutputStream *stream;
  while (outputBuffers.size_approx() > 0) {
    if (outputBuffers.try_dequeue(stream)) {
      delete stream;
    }
  }

  cclient::data::compression::Compressor *compressor = nullptr;

  while (compressors.size_approx() > 0) {
    if (compressors.try_dequeue(compressor) && compressor != nullptr) {
      delete compressor;
    }
  }
  topExists = false;
  rKey = nullptr;
}

}  // namespace data

}  // namespace cclient
