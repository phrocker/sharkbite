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

#pragma once

#include <vector>
#include <future>
#include <deque>
#include <iterator>
#include <memory>
#include <mutex>
#include <thread>

#include "../compressor/compressor.h"
#include "data/streaming/Streams.h"
#include "data/streaming/accumulo/KeyValueIterator.h"
#include "data/streaming/input/InputStream.h"
#include "data/constructs/KeyValue.h"
// meta
#include "meta/MetaBlock.h"
#include "meta/LocalityGroupReader.h"

// bcfile
#include "bcfile/meta_index.h"
#include "bcfile/data_index.h"
#include "bcfile/BlockCompressedFile.h"
#include "SequentialRFile.h"
#include "data/constructs/Range.h"
#include "data/streaming/FileStream.h"
#include "utils/ThreadPool.h"

namespace cclient {
namespace data {

struct GroupedReaders{
  std::vector< std::shared_ptr<cclient::data::streams::KeyValueIterator> > readers;
};

struct ReadersProxy {
  uint16_t maxThread;
  size_t threadCacheCount;
  std::atomic<uint16_t> currentThread;
  std::atomic<bool> delegatorOnline;
  std::mutex delegatorMutex;
  std::condition_variable delegatorCondition;
  std::atomic< moodycamel::ConcurrentQueue<std::shared_ptr<cclient::data::KeyValue>>*> resultSet;
};

class ThreadReaders{
public:
  std::shared_ptr<cclient::data::streams::KeyValueIterator> heapIter;
  std::shared_ptr<cclient::data::streams::StreamRelocation> range = {};
  moodycamel::ConcurrentQueue<std::shared_ptr<cclient::data::KeyValue>> resultSet;
  std::atomic<bool> complete;
  std::atomic<bool> started;
  explicit ThreadReaders(const std::shared_ptr<cclient::data::streams::KeyValueIterator> &itr) : heapIter(itr), complete(false){
    
  }
};

class ParallelRFile : public cclient::data::streams::StreamInterface, public cclient::data::streams::KeyValueIterator  {

 public:

  /**
   Constructor
   @param input stream output stream for the r file
   @param bWriter block compressed writer.
   @param blockSize the desired block size of the bc file.
   **/
  explicit ParallelRFile(std::function<std::vector<cclient::data::streams::FileStream>()> streamfn, int versions=0, bool withDeletes=false, bool propogate =false);

  ParallelRFile(ParallelRFile &&other) = default;

  ParallelRFile& operator=(ParallelRFile &&other) = default;

  virtual ~ParallelRFile();

  virtual void relocate(cclient::data::streams::StreamRelocation *location) override;

  virtual void close() override;


  virtual bool hasNext() override;
  virtual void next() override;

  virtual std::pair<std::shared_ptr<cclient::data::Key>, std::shared_ptr<cclient::data::Value>> operator*();


virtual uint64_t getEntriesFiltered() override;


  virtual std::shared_ptr<Key> getTopKey() override { return current_kv->getKey(); }

  virtual std::shared_ptr<Value> getTopValue() override { return current_kv->getValue(); }


private:

  // found at https://stackoverflow.com/questions/14226952/partitioning-batch-chunk-a-container-into-equal-sized-pieces-using-std-algorithm
  template <typename It> std::vector<std::pair<It,It>> binRanges(It range_from, It range_to, const std::ptrdiff_t num) {
    /* Aliases, to make the rest of the code more readable. */
    using diff_t = std::ptrdiff_t;

    /* Total item number and portion size. */
    const diff_t total
    { std::distance(range_from,range_to) };
    const diff_t portion
    { total / num };

    std::vector<std::pair<It,It>> chunks(num);

    It portion_end
    { range_from };

    /* Use the 'generate' algorithm to create portions. */    
    std::generate(std::begin(chunks),std::end(chunks),[&portion_end,portion]()
          {
            It portion_start
            { portion_end };

            portion_end += portion;
            return std::make_pair(portion_start,portion_end);
          });

    /* The last portion's end must always be 'range_to'. */    
    chunks.back().second = range_to;

    return chunks;
  }


  void startDelegator();

  void stopExecutors();

  std::function<std::vector<cclient::data::streams::FileStream>()> streamfunction;

  std::vector<std::shared_ptr<cclient::data::Range>> keys;

  //std::unique_ptr<cclient::data::streams::InputStream> input_streamlong fileLength

  std::vector<GroupedReaders> readers;

  // non blocking queue of results from a specific thread
  std::vector<std::shared_ptr<ThreadReaders>> thread_readers;

  int versions;
  bool withDeletes;
  bool propogate;

  std::future<size_t> delegatorFuture;
  std::vector<std::future<size_t>> children;

  bool readersEnabled;

  std::atomic<bool> readersRunning;

  std::shared_ptr<cclient::data::KeyValue> current_kv = nullptr;

  std::condition_variable readersConsumerCondition;
  std::condition_variable readersCondition;
  std::mutex readersMutex;
  ReadersProxy readersResult;
  utils::ThreadPool<utils::TaskRescheduleInfo> thread_pool_;
  utils::ThreadPool<utils::TaskRescheduleInfo> delegator_pool_;
  
  std::atomic<int> counter_;
  std::mutex current_counter_;

  /**
   * 
   *  std::function<utils::TaskRescheduleInfo()> f_ex = [serviceNode] {
      serviceNode->enable();
      return utils::TaskRescheduleInfo::Done();
    };

  // only need to run this once.
  auto monitor = utils::make_unique<utils::ComplexMonitor>();
  utils::Worker<utils::TaskRescheduleInfo> functor(f_ex, serviceNode->getUUIDStr(), std::move(monitor));
  // move the functor into the thread pool. While a future is returned
  // we aren't terribly concerned with the result.
  std::future<utils::TaskRescheduleInfo> future;
  thread_pool_.execute(std::move(functor), future);
  if (future.valid())
    future.wait();
  return future;

   * 
   */ 


};
}
}
