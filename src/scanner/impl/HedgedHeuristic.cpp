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

#include "scanner/constructs/HedgedHeuristic.h"

namespace scanners {

void HedgedScannerHeuristic::setTableIterators(std::vector<cclient::data::IterInfo> iters) {
  this->iters = std::move(iters);
}

uint16_t HedgedScannerHeuristic::scan(Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *source) {
  acquireLock();
  std::lock_guard<std::timed_mutex> lock(serverLock, std::adopt_lock);
  if (!started) {
    started = true;
    running = true;
  }
  uint16_t scans = 0;
  for (int i = 0; i < threadCount; i++) {
    ScanPair<interconnect::ThriftTransporter> *pair = new ScanPair<interconnect::ThriftTransporter>;
    pair->src = source;
    pair->heuristic = this;
    pair->runningFlag = &running;
    pair->disableRpc=disableRpc;
    pair->ownedAdditionalFeatures=(void*)&iters;
    threads.push_back(std::thread(HedgedScannerHeuristic::hedgedScan, pair));
  }
  return scans;
}

void HedgedScannerHeuristic::close() {
  running = false;
  std::lock_guard<std::timed_mutex> lock(serverLock);

  if (started) {
    for (std::vector<std::thread>::iterator iter = threads.begin(); iter != threads.end(); iter++) {
      iter->join();
    }
  }
  started = false;
}

}
