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

#include <sys/socket.h>
#include <arpa/inet.h>

#include "interconnect/Scan.h"
#include "interconnect/ClientInterface.h"

namespace interconnect {

Scan::Scan(std::atomic<bool> *isRunning)
    :
    topKey(0),
    isMulti(false),
    isRFile(false) {
  srand(time(NULL));
  scanId = rand();
  this->isRunning = isRunning;
  hasMore = false;
}

Scan::~Scan() {

}

/**
 * Sets the next result set.
 * @param resultSet result set to add to this object's results
 * @returns result of adding.
 **/
bool Scan::setNextResults(std::vector<std::shared_ptr<cclient::data::KeyValue> > *resultSet) {
  results.insert(results.end(), resultSet->begin(), resultSet->end());
  if (!resultSet->empty())
    topKey = resultSet->back()->getKey();
  return true;
}

bool Scan::isClientRunning() {
  return isRunning->load();
}

void Scan::setMultiIterator(std::shared_ptr<cclient::data::streams::KeyValueIterator> &itr) {
  iter = itr;
}

std::shared_ptr<cclient::data::streams::KeyValueIterator> Scan::getMultiIterator() {
  return iter;
}

/**
 * Gets the next result and places it into resultSet
 * @param reference result set to add.
 **/
bool Scan::getNextResults(std::vector<std::shared_ptr<cclient::data::KeyValue> > *resultSet) {
  resultSet->insert(resultSet->end(), results.begin(), results.end());
  results.clear();
  return hasMore;
}

/**
 * Set flag to identify that more results are available.
 * @param more more results
 **/
void Scan::setHasMore(bool more) {
  hasMore = more;
}

/**
 * Gets variable to identify if more results are available
 * @returns whether or not there are more results.
 **/
bool Scan::getHasMore() const {
  return hasMore;

}
/**
 * Returns the scan id
 * @returns scan id.
 **/
int64_t Scan::getId() const {

  return scanId;

}

bool Scan::empty() const {
  return results.empty();
}

void Scan::setMultiScan(bool isMulti) {
  this->isMulti = isMulti;
}

bool Scan::isMultiScan() const {
  return isMulti;
}

void Scan::setRFileScan(bool isRFile) {
  this->isRFile = isRFile;
}

bool Scan::isRFileScan() const {
  return isRFile;
}

/**
 * Sets the scan id
 * @param scanId scan identifier
 **/
void Scan::setScanId(int64_t scanId) {
  this->scanId = scanId;
}

void Scan::setTopKey(std::shared_ptr<cclient::data::Key> key) {
  topKey = key;
}

std::shared_ptr<cclient::data::Key> Scan::getTopKey() const {
  return topKey;
}

}
