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

#ifndef SCANARBITER_H_
#define SCANARBITER_H_
#include <deque>
#include <future>
#include <memory>

#include "Scan.h"

namespace interconnect {

class ScanArbiter {
 private:
  std::mutex m;
  std::condition_variable cv;
  std::deque<Scan*> index;
  std::vector<std::future<Scan*>> futures;
  std::string receivedException;
  uint16_t count;
  uint16_t max;
  bool disableRpc;

 public:
  explicit ScanArbiter(uint16_t desired, bool disableRpc);

  ~ScanArbiter();

  Scan* wait();

  void add(Scan* scan);
};

}  // namespace interconnect
#endif
