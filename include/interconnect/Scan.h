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

#ifndef SCAN_H_
#define SCAN_H_ 1

#include <vector>
#include <atomic>
#include <stdio.h>      /* printf, scanf, puts, NULL */
#include <stdlib.h>     /* srand, rand */
#include <time.h>

#include "../data/constructs/KeyValue.h"
#include "data/streaming/accumulo/KeyValueIterator.h"

//http://sector.sourceforge.net/software.html

/**
 * Represents a running scan
 */

namespace interconnect {

/**
 * Scan representation
 * 
 **/
class Scan {
 public:

  explicit Scan(std::atomic<bool> *isRunning);

  ~Scan();

  /**
   * Sets the next result set.
   * @param resultSet result set to add to this object's results
   * @returns result of adding.
   **/
  bool setNextResults(std::vector<std::shared_ptr<cclient::data::KeyValue> > *resultSet);

  bool isClientRunning();

  void setMultiIterator(std::shared_ptr<cclient::data::streams::KeyValueIterator> &itr);

  std::shared_ptr<cclient::data::streams::KeyValueIterator> getMultiIterator();

  /**
   * Gets the next result and places it into resultSet
   * @param reference result set to add.
   **/
  bool getNextResults(std::vector<std::shared_ptr<cclient::data::KeyValue> > *resultSet);

  /**
   * Set flag to identify that more results are available.
   * @param more more results
   **/
  void setHasMore(bool more);

  /**
   * Gets variable to identify if more results are available
   * @returns whether or not there are more results.
   **/
  bool getHasMore() const;
  /**
   * Returns the scan id
   * @returns scan id.
   **/
  int64_t getId() const;

  void setMultiScan(bool isMulti);
  bool isMultiScan() const;

  void setRFileScan(bool isRFile);
  bool isRFileScan() const;

  bool empty() const;

  void setException(std::string exc);

  std::string getException() const;

  bool hasException() const;

  /**
   * Sets the scan id
   * @param scanId scan identifier
   **/
  void setScanId(int64_t scanId);

  void setTopKey(std::shared_ptr<cclient::data::Key> key);

  std::shared_ptr<cclient::data::Key> getTopKey() const;

 protected:

  std::string clientException;

  std::shared_ptr<cclient::data::streams::KeyValueIterator> iter;

  bool isRFile;

  bool isMulti;

  std::atomic<bool> *isRunning;

  std::shared_ptr<cclient::data::Key> topKey;
  // scan id
  int64_t scanId;
  // has more results.
  bool hasMore;
  // results
  std::vector<std::shared_ptr<cclient::data::KeyValue> > results;
};
}
#endif /* SCAN_H_ */
