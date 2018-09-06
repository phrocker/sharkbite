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
#include "writer/impl/WriterHeuristic.h"

namespace writer {

WriterHeuristic::WriterHeuristic(short numThreads, uint32_t queueSize)
    : threadCount(numThreads),
      started(false),
      queue(queueSize) {
  closed = false;

  conditionals = new SinkConditions();

}

WriterHeuristic::~WriterHeuristic() {
  close();
  /*
  WritePair *pair = NULL;
  while (conditionals->getMutationCount() > 0) {
    if (queue.try_dequeue(pair)) {
      conditionals->decrementMutationCount();
      delete pair->rangeDef;
      delete pair->mutations;

    }
  }*/
  delete conditionals;
}

} /* namespace data */
