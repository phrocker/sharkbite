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
 * See the License for the specific s governing permissions and
 * limitations under the License.
 */

#ifndef RESULTS_H_
#define RESULTS_H_

#include <atomic>
#include <iostream>
#include <iterator>
#include <set>
#include <stdexcept>
#include <string>
#ifdef PYTHON
#include <pybind11/pybind11.h>
#endif
#include "../../data/constructs/inputvalidation.h"
#include "SourceConditions.h"
#include "data/extern/concurrentqueue/concurrentqueue.h"

namespace scanners {
template <typename T>
class ResultBlock;
template <typename T>
class ResultIter;
template <typename T, class BlockType>
class Results;

/**

 **/
template <typename T>
// class ResultBlock : public std::iterator<std::forward_iterator_tag, T> {
class ResultBlock : public std::istream_iterator<T> {
 protected:
  SourceConditions *sourceConditionals;

  moodycamel::ConcurrentQueue<std::shared_ptr<T>> *resultSet;

  std::shared_ptr<T> current;

  bool isEnd;

  std::exception_ptr result_excp;

  virtual void setEnd(bool val) { isEnd = val; }

 public:
  ResultBlock(SourceConditions *conditionals,
              moodycamel::ConcurrentQueue<std::shared_ptr<T>> *queue,
              bool setEnd = false)
      : isEnd(setEnd), result_excp(nullptr) {
    resultSet = queue;
    sourceConditionals = conditionals;
  }

  ResultBlock() : result_excp(nullptr) {}

  ResultBlock<T> begin() {
    getNextResult();
    return *this;
  }

  bool isEndOfRange() const { return isEnd; }

  void setException(const std::exception_ptr &e) { result_excp = e; }

  ResultBlock<T> end() {
    return static_cast<ResultBlock<T>>(
        ResultBlock(sourceConditionals, resultSet, true));
  }

  SourceConditions *getSourceConditionals() const { return sourceConditionals; }

  moodycamel::ConcurrentQueue<std::shared_ptr<T>> *getResultSet() const {
    return resultSet;
  }

  std::shared_ptr<T> get() { return current; }

  std::shared_ptr<T> operator*() { return current; }

  inline void getNextResult() {
    do {
      if (!resultSet->try_dequeue(current)) {
        sourceConditionals->waitForResults();
        if (resultSet->try_dequeue(current)) {
          sourceConditionals->decrementCount();
          break;
        }
        if (!sourceConditionals->isAlive() && sourceConditionals->size() <= 0) {
          isEnd = true;
        }

      } else {
        sourceConditionals->decrementCount();
        break;
      }
    } while (sourceConditionals->isAlive() && !result_excp);
    if (result_excp) {
      std::rethrow_exception(result_excp);
    }
  }
  ResultBlock &operator++() {
    getNextResult();
    return *this;
  }

  ResultBlock &operator++(int t) {
    for (int i = 0; i < (t + 1); i++) {
      getNextResult();
    }

    return *this;
  }

  ResultBlock &operator=(const ResultBlock &rhs) {
    resultSet = rhs.resultSet;
    sourceConditionals = rhs.sourceConditionals;
    isEnd = rhs.isEnd;
    return *this;
  }

  bool operator==(const ResultIter<std::unique_ptr<T>> &rhs) {
    return isEnd == rhs.getParent();
  }

  bool operator!=(const ResultBlock &rhs) { return !(isEnd == rhs.isEnd); }

  void add(T *t) {
    resultSet->enqueue(t);

    sourceConditionals->awakeThreadsForResults();
  }

  void add(std::vector<std::unique_ptr<T>> *t) {
    for (typename std::vector<std::unique_ptr<T>>::iterator it = t->begin();
         it != t->end(); ++it) {
      resultSet->enqueue(*it.release());
      sourceConditionals->incrementCount();
    }

    sourceConditionals->awakeThreadsForResults();
  }

  void add(std::vector<std::shared_ptr<T>> *t) {
    for (typename std::vector<std::shared_ptr<T>>::iterator it = t->begin();
         it != t->end(); ++it) {
      resultSet->enqueue(*it);
      sourceConditionals->incrementCount();
    }

    sourceConditionals->awakeThreadsForResults();
  }

  virtual ~ResultBlock() {}
  friend class ResultIter<std::shared_ptr<T>>;
};

template <typename T>
class ResultIter : public ResultBlock<T> {
 protected:
  ResultBlock<T> *parent;

  void setEnd(bool val) { ResultBlock<T>::setEnd(val); }

 public:
  ResultIter(ResultBlock<T> *copyResultSet, bool end = false)
      : ResultBlock<T>(copyResultSet->getSourceConditionals(),
                       copyResultSet->getResultSet(),
                       (end ? end : copyResultSet->isEndOfRange()))

  {
    setEnd((end ? end : copyResultSet->isEndOfRange()));
    parent = copyResultSet;
  }

  ResultIter(ResultBlock<T> &copyResultSet, bool end = false)
      : ResultBlock<T>(copyResultSet.getSourceConditionals(),
                       copyResultSet.getResultSet(),
                       (end ? end : copyResultSet.isEndOfRange()))

  {
    setEnd((end ? end : copyResultSet.isEndOfRange()));
    parent = this;
  }

  ResultIter(ResultIter<T> *copyResultSet, bool end = false)
      : ResultBlock<T>(copyResultSet->getSourceConditionals(),
                       copyResultSet->getResultSet(),
                       (end ? end : copyResultSet->isEndOfRange()))

  {
    setEnd((end ? end : copyResultSet->isEndOfRange()));
    parent = copyResultSet;
  }

  ResultIter(ResultIter<T> &copyResultSet, bool end = false)
      : ResultBlock<T>(copyResultSet.getSourceConditionals(),
                       copyResultSet.getResultSet(),
                       (end ? end : copyResultSet.isEndOfRange()))

  {
    setEnd((end ? end : copyResultSet.isEndOfRange()));
    parent = this;
  }

  ResultBlock<T> begin() { return *this; }

  ResultBlock<T> end() { return parent->end(); }

  ResultBlock<T> *getParent() { return parent; }

  std::unique_ptr<T> operator*() { return std::move((parent->operator*())); }

  ResultIter &operator++() {
    (*parent)++;
    return *this;
  }

  ResultIter &operator++(int t) {
    (*parent)++;
    return *this;
  }

  bool isEndOfRange() const { return parent->isEndOfRange(); }

  bool operator==(const ResultIter &rhs) {
    return *(*parent) == *(*rhs.parent);
  }

  bool operator==(const ResultBlock<T> &rhs) {
    return parent->isEndOfRange() == rhs.isEndOfRange();
  }

  bool operator!=(const ResultIter<T> &rhs) { return !(*this == rhs); }

  bool operator!=(const ResultBlock<T> &rhs) { return !(*this == rhs); }

  virtual ~ResultIter() {}
};

template <typename T, class BlockType>
class Results {
 protected:
  moodycamel::ConcurrentQueue<std::shared_ptr<T>> resultSet;

  std::unique_ptr<BlockType> iter;

  SourceConditions conditions;

  volatile uint16_t producers;

 public:
  typedef BlockType iterator;

  Results() : resultSet(2000) {
    iter = std::unique_ptr<BlockType>(new BlockType(&conditions, &resultSet));
    producers = 0;
  }

  void setException(std::exception_ptr e) { iter->setException(e); }

  void add(T *t) { iter->add(t); }

  void add_ptr(std::vector<std::shared_ptr<T>> *t) { iter->add(t); }

  bool isEndOfRange() const { return iter->isEndOfRange(); }
#ifdef PYTHON
  auto next() {
    if (!iter->isEndOfRange()) {
      const auto &res = iter->get();
      (*iter)++;
      return res;
    }

    throw pybind11::stop_iteration();
  }
#endif
  iterator begin() { return iter->begin(); }

  iterator end() { return iter->end(); }

  void decrementProducers() {
    if (--producers <= 0) {
      conditions.awakeThreadsFinished();
    }
  }

  void registerProducer() { producers++; }

  ~Results() {}

 protected:
};
} /* namespace scanners */
#endif /* RESULTS_H_ */
