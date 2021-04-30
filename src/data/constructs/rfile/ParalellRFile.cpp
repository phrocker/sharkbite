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

#include <algorithm>
#include <utility>

#include "data/constructs/rfile/ParallelRFile.h"
#include "data/iterators/DeletingMultiIterator.h"
#include "data/iterators/MultiIterator.h"
#include "data/iterators/VersioningIterator.h"
#include "data/streaming/accumulo/StreamSeekable.h"

namespace cclient {
namespace data {

ParallelRFile::ParallelRFile(
    std::function<std::vector<cclient::data::streams::FileStream>()> streamfn,
    int versions, bool withDeletes, bool propogate)
    : streamfunction(streamfn),
      versions(versions),
      withDeletes(withDeletes),
      propogate(propogate),
      thread_pool_(1, false, "ParallelRFile threadpool"),
      delegator_pool_(2, false, "Delegator threadpool") {
  readersRunning = false;
}

ParallelRFile::~ParallelRFile() {}

void ParallelRFile::close() {
  // make sure we have stopped all executors
  stopExecutors();
}

void ParallelRFile::stopExecutors() {
  if (readersRunning) {
    readersRunning = false;
    thread_pool_.shutdown();
    delegator_pool_.shutdown();
    readersRunning = false;
    for (uint16_t i = 0; i < readersResult.maxThread; i++) {
      std::shared_ptr<ThreadReaders> my_reader = thread_readers.at(i);
      while (!my_reader->complete)
        std::this_thread::sleep_for(std::chrono::milliseconds(2));
    }
  }
}

void ParallelRFile::relocate(
    const std::shared_ptr<cclient::data::streams::StreamRelocation> &location) {
  close();
  uint32_t number = location->getDesiredThreads();
  if (number == 0) number = 1;

  if (!location->getRange()->getInfiniteStartKey() &&
      !location->getRange()->getInfiniteStopKey()) {
    number = 1;
  }

  thread_pool_.setMaxConcurrentTasks(16);

  thread_pool_.start();

  delegator_pool_.start();

  std::vector<std::shared_ptr<cclient::data::Key>> key_vectors;

  std::vector<cclient::data::streams::FileStream> filestreams =
      streamfunction();
  {
    GroupedReaders rdrs;
    for (auto &filestream : filestreams) {
      auto stream = std::make_shared<cclient::data::SequentialRFile>(
          std::move(filestream.stream), (long)filestream.filesize);
      std::vector<std::shared_ptr<cclient::data::Key>> ks =
          stream->getBlocks(location);
      key_vectors.assign(ks.begin(), ks.end());
      rdrs.readers.push_back(stream);
    }
    readers.emplace_back(rdrs);
  }
  // sort and unique. while this could take a while for very large RFiles, this
  // should be lower than the aggregate
  if (filestreams.size() > 1) {
    std::sort(key_vectors.begin(), key_vectors.end());
    auto last = std::unique(key_vectors.begin(), key_vectors.end());
    key_vectors.erase(last, key_vectors.end());
  }

  // after we've sorted and ensured we have a defined set of ranges we need to
  // re-open RFiles to match the ranges.
  for (uint32_t i = 1; i < number; i++) {
    filestreams = streamfunction();
    GroupedReaders rdrs;
    for (auto &filestream : filestreams) {
      auto stream = std::make_shared<cclient::data::SequentialRFile>(
          std::move(filestream.stream), (long)filestream.filesize);
      rdrs.readers.push_back(stream);
    }
    readers.emplace_back(rdrs);
  }
  // create heap iterators so we can read files within a given thread
  for (const auto &reader_set : readers) {
    std::shared_ptr<cclient::data::streams::KeyValueIterator> heapItr = {};
    if (versions == 0) {
      if (!withDeletes) {
        heapItr =
            std::make_shared<cclient::data::MultiIterator>(reader_set.readers);
      } else {
        heapItr = std::make_shared<cclient::data::DeletingMultiIterator>(
            reader_set.readers, propogate);
      }
    } else {
      heapItr = std::make_shared<cclient::data::VersioningIterator>(
          reader_set.readers);
    }
    thread_readers.emplace_back(std::make_shared<ThreadReaders>(heapItr));
  }

  // partition the keys into ranges.

  if (number > key_vectors.size()) {
    number = key_vectors.size();
  }
  if (location->getRange()->getInfiniteStartKey() &&
      location->getRange()->getInfiniteStopKey()) {
    auto binnedRanges =
        binRanges(key_vectors.begin(), key_vectors.end(), number - 1);

    /**
     * TODO: Currently this only supports (-inf,+inf). This should be extended
     **/
    keys.emplace_back(std::make_shared<cclient::data::Range>(
        *binnedRanges.at(0).first, false));
    for (const auto &pair : binnedRanges) {
      // create a range
      if (*pair.second != nullptr) {
        keys.emplace_back(std::make_shared<cclient::data::Range>(
            *pair.first, true, *pair.second, false));
      } else {
        keys.emplace_back(
            std::make_shared<cclient::data::Range>(true, *pair.first));
      }
    }
  } else {
    keys.emplace_back(std::make_shared<cclient::data::Range>(
        location->getRange()->getStartKey(),
        location->getRange()->getStartKeyInclusive(),
        location->getRange()->getStopKey(),
        location->getRange()->getStopKeyInclusive()));
  }
  assert(keys.size() == thread_readers.size() && thread_readers.size() == number);

  for (uint32_t i = 0; i < number; i++) {
    thread_readers.at(i)->range =
        std::make_shared<cclient::data::streams::DelegatedStreamSeekable>(
            keys.at(i), location);
  }

  readersResult.maxThread = number;
  readersResult.threadCacheCount = location->getKeysCachedPerThread();

  // start a delegator that delegate the ranges to the  worker threads, keeping
  // in mind that data must come in order from those worker threads
  startDelegator();
  // 0 , 1 , 2 , ... n

  // pull data from 0, if empty go to one, allowing zero to take next range,
  // same with one, two , etc until n, at which point we'll return to 0 and
  // start all over. We'll use a circular queue of ranges.
  hasNext();
  next();
}

bool ParallelRFile::hasNext() {
  while (readersResult.resultSet.load()->size_approx() == 0 && readersRunning) {
    std::this_thread::sleep_for(std::chrono::milliseconds(2));
  }
  if (readersResult.resultSet.load()->size_approx() > 0) {
    return true;
  }
  return false;
}
void ParallelRFile::next() {
  if (!readersResult.resultSet.load()->try_dequeue(current_kv)) {
    current_kv = nullptr;
  }
}

std::pair<std::shared_ptr<cclient::data::Key>,
          std::shared_ptr<cclient::data::Value>>
ParallelRFile::operator*() {
  // readersResult.resultSet.try_dequeue(current_kv);
  return std::make_pair(current_kv->getKey(), current_kv->getValue());
}

void ParallelRFile::startDelegator() {
  if (readersRunning) return;

  readersResult.delegatorOnline = false;
  counter_ = 0;
  for (uint16_t i = 0; i < readersResult.maxThread; i++) {
    thread_readers.at(i)->started = false;
    thread_readers.at(i)->complete = false;

    std::function<utils::TaskRescheduleInfo()> f_ex = [&] {
      uint16_t index = 0;
      {
        std::lock_guard<std::mutex> lock(current_counter_);
        index = counter_;
        thread_readers.at(index)->started = true;
        counter_ = index + 1;
      }
      while (!readersResult.delegatorOnline) {
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
      }

      std::shared_ptr<ThreadReaders> my_reader = thread_readers.at(index);
      my_reader->heapIter->relocate(my_reader->range);
      while (readersRunning && my_reader->heapIter->hasNext()) {
        while (readersRunning && !my_reader->resultSet.try_enqueue(
                                     my_reader->heapIter->getTop())) {
          std::this_thread::sleep_for(std::chrono::milliseconds(1));
        }
        while (readersRunning && (readersResult.currentThread != index &&
                                  my_reader->resultSet.size_approx() >
                                      readersResult.threadCacheCount)) {
          std::this_thread::sleep_for(std::chrono::milliseconds(2));
        }
        my_reader->heapIter->next();
      }
      my_reader->complete = true;
      std::unique_lock<std::mutex> lock(readersResult.delegatorMutex);
      readersResult.delegatorCondition.notify_one();
      lock.unlock();
      return utils::TaskRescheduleInfo::Done();
    };

    // only need to run this once.
    auto monitor = std::make_unique<utils::ComplexMonitor>();
    utils::Worker<utils::TaskRescheduleInfo> functor(f_ex, std::to_string(i),
                                                     std::move(monitor));

    std::future<utils::TaskRescheduleInfo> future;
    thread_pool_.execute(std::move(functor), future);
  }

  std::function<utils::TaskRescheduleInfo()> d_ex = [&] {
    readersResult.currentThread = 0;
    readersRunning = true;

    std::shared_ptr<cclient::data::KeyValue> current_key_value;
    // iterate through the threads and pull the data accordingly
    for (int i = 0; i < readersResult.maxThread && readersRunning; i++) {
      readersResult.currentThread = i;
      std::shared_ptr<ThreadReaders> rdr = thread_readers.at(i);
      readersResult.resultSet.store(&rdr->resultSet);
      if (i == 0) {
        readersResult.delegatorOnline = true;  // enable
      }

      std::unique_lock<std::mutex> lock(readersResult.delegatorMutex);
      readersResult.delegatorCondition.wait(
          lock, [&] { return !readersRunning || rdr->complete; });
      lock.unlock();

      while (readersRunning && rdr->resultSet.size_approx() > 0) {
        std::this_thread::sleep_for(std::chrono::milliseconds(2));
      }
    }
    // finished
    readersRunning = false;
    return utils::TaskRescheduleInfo::Done();
  };

  // only need to run this once.
  auto monitor = std::make_unique<utils::ComplexMonitor>();
  utils::Worker<utils::TaskRescheduleInfo> functor(d_ex, "KeyDelegator",
                                                   std::move(monitor));

  std::future<utils::TaskRescheduleInfo> future;
  delegator_pool_.execute(std::move(functor), future);

  while (!readersResult.delegatorOnline) {
    // a long sleep isn't needed here and avoids starvation on
    // single CPU systems
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
}

uint64_t ParallelRFile::getEntriesFiltered() {
  uint64_t filtered = 0;
  for (uint16_t i = 0; i < readersResult.maxThread; i++) {
    std::shared_ptr<ThreadReaders> my_reader = thread_readers.at(i);
    while (!my_reader->complete)
      std::this_thread::sleep_for(std::chrono::milliseconds(2));
    filtered += my_reader->heapIter->getEntriesFiltered();
  }
  return filtered;
}

}  // namespace data
}  // namespace cclient
