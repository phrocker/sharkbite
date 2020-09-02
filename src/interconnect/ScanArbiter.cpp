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

#include "interconnect/ScanArbiter.h"

#include "data/exceptions/ClientException.h"
namespace interconnect {

ScanArbiter::ScanArbiter(uint16_t desired, bool disableRpc)
    : max(desired), disableRpc(disableRpc) {}

ScanArbiter::~ScanArbiter() {
  for (auto scan : index) {
    delete scan;
  }
}
Scan *ScanArbiter::wait() {
  std::unique_lock<std::mutex> lk(m);
  bool hasResult = false;
  Scan *r = nullptr;
  do {
    cv.wait(lk, [this] { return !index.empty(); });
    r = index.front();
    index.pop_front();
    if (r->hasException()) {
      if (!receivedException.empty()) {
        hasResult = false;
        receivedException = r->getException();
      } else {
        throw cclient::exceptions::ClientException(receivedException);
      }
    } else if (r->isRFileScan() && (!disableRpc && r->empty())) {
      hasResult = false;
    } else {
      hasResult = true;
    }
  } while (!hasResult);
  lk.unlock();
  return r;
}

void ScanArbiter::add(Scan *scan) {
  {
    std::unique_lock<std::mutex> lk(m);

    index.push_back(scan);

    lk.unlock();
  }
  cv.notify_one();
}

}  // namespace interconnect
