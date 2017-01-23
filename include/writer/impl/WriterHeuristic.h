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
#ifndef SRC_WRITER_IMPL_WRITERHEURISTIC_H
#define SRC_WRITER_IMPL_WRITERHEURISTIC_H
#include "../../scanner/constructs/Heuristic.h"
#include "data/extern/concurrentqueue/concurrentqueue.h"
#include "../../data/constructs/server/ServerDefinition.h"
#include "../SinkConditionals.h"
#include "../../interconnect/TabletServer.h"

#include <thread>
#include <vector>
#include <atomic>
#include <mutex>

namespace writer {


struct WritePair {
    //ServerInterconnect *interconnect;
    cclient::data::tserver::ServerDefinition *rangeDef;
    const cclient::impl::Configuration *conf;
    cclient::data::TabletServerMutations *mutations;
    void *ref;
};

/*
 *
 */
class WriterHeuristic: public scanners::Heuristic<interconnect::ThriftTransporter> {
public:

    WriterHeuristic(short numThreads = 10, uint32_t queueSize=2000);
    /**
     * Add a server interconnect
     */
    void addClientInterface(
        interconnect::ClientInterface<interconnect::ThriftTransporter> *serverIfc) {

        Heuristic::addClientInterface(serverIfc);
    }

    uint16_t write(//ServerInterconnect *interConnect,
		   cclient::data::tserver::ServerDefinition *rangeDef,
		   const cclient::impl::Configuration *conf,
                   cclient::data::TabletServerMutations *mutations) {

        if (!started) {
	    std::lock_guard<std::mutex> lock(serverLock);
            if (!started) {
                for (int i = 0; i < threadCount; i++) {
		  threads.push_back( std::thread(WriterHeuristic::write_thrift,this) );
                }
            }
            started = true;
        }

        WritePair *pair = new WritePair;
	pair->conf = conf;
	pair->ref = this;
	pair->rangeDef = rangeDef;
	mutations->setMaxFailures(2);
        pair->mutations = mutations;
	
	while(!queue.try_enqueue(pair))
	{
	  if (!conditionals->isAlive())
	    throw std::runtime_error("Closed during write");
	}
	conditionals->incrementMutationCount();

        conditionals->awakeThreadsForResults();
        return conditionals->getMutationCount();
    }

    int close() {

      std::lock_guard<std::mutex> lock(serverLock);
      if (failedMutations.size() > 0)
      {
	return 1;
      }
      else if (threads.size() == 0)
      {
	return 0;
      }
      else{
	  if (closed)
	  {
	    return 0;
	  }
	  
	

	  if (started)
	  {
	    conditionals->awakeThreadsFinished();
	    
	      for (std::vector<std::thread>::iterator iter = threads.begin(); iter != threads.end(); iter++)
	    {
		    iter->join();
	    }
	  
	  }
	  conditionals->close();
	  closed = true;
	  return 0;
      }
    }

     uint64_t maxThreads()
    {
      return threadCount;
    }
    
    uint64_t size()
    {
      return conditionals->getMutationCount();
    }
    virtual ~WriterHeuristic();
    
    void restart_failures(std::vector<cclient::data::Mutation*> *mutations)
    {
      std::lock_guard<std::mutex> lock(serverLock);
      mutations->insert(mutations->end(),failedMutations.begin(),failedMutations.end());
      failedMutations.clear(); 
    }
      void addFailedMutation(cclient::data::TabletServerMutations *mutation)
    {
      std::lock_guard<std::mutex> lock(serverLock);
      std::map<cclient::data::KeyExtent*, std::vector<cclient::data::Mutation*> > *mutationMap = mutation->getMutations();
      for(auto entry : *mutationMap)
      {
	failedMutations.insert(failedMutations.end(),entry.second.begin(),entry.second.end());
	entry.second.clear();
      }
      
    }
    
     void push_failures(std::vector<cclient::data::Mutation*> *mutations)
    {
      std::lock_guard<std::mutex> lock(serverLock);
      failedMutations.insert(failedMutations.end(),mutations->begin(),mutations->end());
    }
protected:

    static void *write_thrift(WriterHeuristic *heuristic) {
      

	WritePair *pair = NULL;
        do {
            pair = heuristic->next();
	    
            if (NULL != pair) {
                interconnect::ServerInterconnect *conn=
		        new interconnect::ServerInterconnect (
		        pair->rangeDef, pair->conf);

                if (NULL != conn) {

                    if (conn->write(pair->mutations) != NULL)
		    {
			  // take failed mutations back so we can try later on
			  ((WriterHeuristic*) pair->ref)->addFailedMutation(pair->mutations);
		    }
		  
		    delete pair->mutations;
                    delete pair->rangeDef;
		    delete conn;
                }

                delete pair;
            } else {
                break;
            }
            

        } while (NULL != pair);

        return 0;
    }

    virtual WritePair *next() {
        WritePair *pair = NULL;
	
	if (!conditionals->isAlive())
	{
	  return pair;
	}

        do {
            if (!queue.try_dequeue(pair)) {
                conditionals->waitForResults();
                if (queue.try_dequeue<>(pair)) {
		  conditionals->decrementMutationCount();
                    break;
                }
                if (conditionals->isClosing()) {
                    return NULL;
                }

            }
            else
	    {
	      conditionals->decrementMutationCount();
	      break;
	    }
            
        } while (conditionals->isAlive());

        return pair;

    }
    

  

    volatile bool started;
    //boost::lockfree::queue<WritePair*, boost::lockfree::fixed_sized<false>> queue;
    moodycamel::ConcurrentQueue<WritePair*> queue;
private:
    SinkConditions *conditionals;
    std::vector<cclient::data::Mutation*> failedMutations;
    std::mutex serverLock;
    std::vector<std::thread> threads;
    uint16_t threadCount;
    volatile bool closed;
}
;

} /* namespace data */

#endif /* SRC_WRITER_IMPL_WRITERHEURISTIC_H_ */
