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

#ifndef SRC_SCANNER_CONSTRUCTS_SINKCONDITIONS_H_
#define SRC_SCANNER_CONSTRUCTS_SINKCONDITIONS_H_

#include <thread>
#include <mutex>
#include <atomic>
#include <condition_variable>
namespace scanners {
/**
 * Class that contains the conditions for connectivity
 * within the heuristic classes.

 **/
class SourceConditions {
public:
    /**
     * Constructor that initializes the conditions and mutexes.
     **/
    SourceConditions() {
        alive = true;
	num_results=0;
    }

    /**
     * waits on results or the completion of the heuritic scan
     **/
    void waitForResults() {

         std::unique_lock<std::recursive_mutex> lock(resultMutex);
        if (alive ) {
	    moreResults.wait(lock, [&](){return !this->alive||this->num_results>0;});
        }
    }

    /**
     * Awakes threads that are awaiting results.
     * broadcasts to all threads waiting on the condition.
     **/
    void awakeThreadsForResults() {
      std::lock_guard<std::recursive_mutex> lock(resultMutex);
	moreResults.notify_all();
    }

    /**
     * Determines if the source condition is still alive.
     **/
    bool isAlive() {
        return alive;
    }

    void awakeThreadsFinished() {
      std::lock_guard<std::recursive_mutex> lock(resultMutex);
        alive = false;
        awakeThreadsForResults();

    }
    
    inline void incrementCount()
    {
      ++num_results;
    }
    
    inline void decrementCount()
    {
      num_results--;
    }
    
    inline int size()
    {
      return num_results;
    }
    
protected:
    volatile bool alive;
    std::atomic_int num_results;
    std::condition_variable_any moreResults;
    std::recursive_mutex resultMutex;

};

}
#endif /* SRC_SCANNER_CONSTRUCTS_SINKCONDITIONS_H_ */
