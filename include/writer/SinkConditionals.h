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
#ifndef SRC_SCANNER_CONSTRUCTS_SOURCECONDITIONS_H_
#define SRC_SCANNER_CONSTRUCTS_SOURCECONDITIONS_H_



#include <thread>
#include <mutex>
#include <condition_variable>

namespace writer {

class SinkConditions {
public:
    SinkConditions() {
        mutation_count = 0;
        alive = true;
	closing = false;
    }

    void waitForResults() {
        std::unique_lock<std::recursive_mutex> lock(resultMutex);
        if (alive && !closing) {
	    moreResults.wait(lock, [&](){return !this->alive||this->closing||this->mutation_count>0;});
        }
    }

    void awakeThreadsForResults() {
      std::lock_guard<std::recursive_mutex> lock(resultMutex);
        moreResults.notify_all();
    }

    bool isAlive() {
        return alive;
    }
    
    bool isClosing()
    {
      return closing;
    }
    
    void close()
    {
      closing = false;
      alive = true;
    }
    
    void incrementMutationCount(uint32_t count=1)
    {
      mutation_count+= count;
    }
    
    uint64_t getMutationCount()
    {
      return mutation_count;
    }
    
    void decrementMutationCount(uint32_t count=1)
    {
      mutation_count-=count;
    }

    void awakeThreadsFinished() {
        std::lock_guard<std::recursive_mutex> lock(resultMutex);
        closing = true;
        awakeThreadsForResults();
    }
protected:
    std::atomic_long mutation_count;
    volatile bool closing;
    volatile bool alive;
    std::condition_variable_any moreResults;
    std::recursive_mutex resultMutex;

};

}
#endif /* SRC_SCANNER_CONSTRUCTS_SOURCECONDITIONS_H_ */
