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

#ifndef SRC_WRITER_SINK_H_
#define SRC_WRITER_SINK_H_

#include "data/constructs/Mutation.h"
#include <chrono>
#include <memory>
#include <thread>
#include "data/extern/concurrentqueue/concurrentqueue.h"
#include <atomic>
namespace writer {

template<class T>
class Sink {

 protected:

  moodycamel::ConcurrentQueue<std::shared_ptr<T>> sinkQueue;

  virtual bool
  exceedQueue();

  virtual bool
  enqueue(std::shared_ptr<T> obj);

  uint16_t queueSize;

  virtual uint64_t maxWait() {
    return 0;
  }

  virtual uint64_t waitingSize() {
    return 0;
  }

 public:

  Sink(uint16_t maxQueue)
      : queueSize(maxQueue),
        sinkQueue((maxQueue * 1.5)) {

  }

  virtual ~Sink() {
  }

  /**
   * Method to put object onto the queue
   * @param obj incoming object to push into the sink
   */
  bool
  push(std::unique_ptr<T> obj);

  /**
   * Method to put object onto the queue
   * @param obj incoming object to push into the sink
   */
  bool
  push(std::shared_ptr<T> obj);

  /**
   * Flushes the sink
   */
  virtual void
  flush(bool override = false) = 0;

  /**
   Add a mutation, transferring ownership to this sink
   **/
  virtual bool addMutation(const std::shared_ptr<cclient::data::Mutation> &mut) = 0;

  /**
   Add a mutation
   **/
  virtual bool
  addMutation(std::unique_ptr<cclient::data::Mutation> obj) = 0;

  /**
   * Closes the sink
   */
  virtual void close() {
    flush(true);
  }

  virtual void exit() {
    flush(true);
  }

  inline virtual size_t size() {

    return sinkQueue.size_approx();
  }

};

/**
 * Method to put object onto the queue
 * @param obj incoming object to push into the sink
 */
template<typename T>
bool Sink<T>::push(std::unique_ptr<T> obj) {

  while (waitingSize() >= ((maxWait() + 1) * 1.5)) {
    std::this_thread::sleep_for(std::chrono::milliseconds(25));
  }
  /**
   * If enqueue r
   */
  std::shared_ptr<T> ptr = std::move(obj);
  if (enqueue(ptr) && exceedQueue()) {
    flush();
  }

  return true;
}

template<typename T>
bool Sink<T>::push(std::shared_ptr<T> obj) {

  while (waitingSize() >= ((maxWait() + 1) * 1.5)) {
    std::this_thread::sleep_for(std::chrono::milliseconds(25));
  }

  if (enqueue(obj) && exceedQueue()) {
    flush();
  }

  return true;
}

/**
 * Method to put object onto the queue
 * @param obj incoming object to push into the sink
 */
template<typename T>
bool Sink<T>::enqueue(std::shared_ptr<T> obj) {
  bool enqueued = sinkQueue.enqueue(obj);

  return enqueued;

}

/**
 * Method to put object onto the queue
 * @param obj incoming object to push into the sink
 */
template<typename T>
bool Sink<T>::exceedQueue() {
  if (size() > queueSize) {
    return true;
  }

  return false;

}

} /* namespace writer */

#endif /* SRC_WRITER_SINK_H_ */
