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
#include "../../../include/writer/impl/SinkImpl.h"

#include "../../../include/writer/impl/../../data/constructs/KeyValue.h"
#include "../../../include/writer/impl/../../data/constructs/Mutation.h"
#include "../../../include/writer/impl/../../data/exceptions/ClientException.h"
#include "../../../include/writer/impl/../../data/constructs/Key.h"
#include "../../../include/writer/impl/../../data/constructs/value.h"
#include "../../../include/writer/impl/WriterHeuristic.h"

namespace writer{
Writer::Writer (cclient::data::Instance *instance,
                    interconnect::TableOperations<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *tops,
                    cclient::data::security::Authorizations *auths, uint16_t threads) : tops(tops),
	Sink<cclient::data::KeyValue> (500), mutationQueue(500*1.5)
{
	connectorInstance = dynamic_cast<cclient::data::zookeeper::ZookeeperInstance*> (instance);
	tableLocator = cclient::impl::cachedLocators.getLocator (
	                       cclient::impl::LocatorKey (connectorInstance, tops->getTableId ()));
	credentials = tops->getCredentials ();
	writerHeuristic = new WriterHeuristic (threads);
}

Writer::~Writer ()
{
	if (writerHeuristic->close() > 0)
	{
	  std::vector<cclient::data::Mutation*> failures;
	  writerHeuristic->restart_failures(&failures);
	  handleFailures(&failures);
	  flush(true);
	}
	delete writerHeuristic;
}
void
Writer::handleFailures(std::vector<cclient::data::Mutation*> *failures)
{
  std::vector<cclient::data::Mutation*> newFailures;
  
  std::map<std::string, cclient::data::TabletServerMutations*> binnedMutations;
	std::set<std::string> locations;
	
	
  tableLocator->binMutations (credentials, failures, &binnedMutations,
	                            &locations, &newFailures);
	for (std::string location : locations) {
		std::vector<std::string> locationSplit = split (location, ':');
		cclient::data::tserver::ServerDefinition *rangeDef = new cclient::data::tserver::ServerDefinition (
		        credentials,
		        NULL,
		        locationSplit.at (0), atoi (locationSplit.at (1).c_str ()));
		writerHeuristic->write (rangeDef, connectorInstance->getConfiguration(), binnedMutations.at (location));

	}
	
	writerHeuristic->push_failures(failures);
	
	
}
void
Writer::flush (bool override)
{
	std::vector<cclient::data::Mutation*> failures;
	writerHeuristic->restart_failures(&failures);
	handleFailures(&failures);
	while((currentSize+mutationQueue.size_approx()) > 0)
	{
	cclient::data::KeyValue **kv = new cclient::data::KeyValue*[queueSize];
	//size_t dequeued = sinkQueue->try_dequeue_bulk (kv, queueSize);
	size_t dequeued = 0;
	for(uint32_t i  = 0; i < queueSize; )
	{
	  if (sinkQueue.pop(kv[i]))
	  {
	    currentSize--;
	    dequeued++;
	    i++;
	  }
	  else
	    break;
	}
	cclient::data::Mutation *prevMutation = NULL;
	std::vector<cclient::data::Mutation*> *mutation = new std::vector<cclient::data::Mutation*> ();
	for (uint64_t i = 0; i < dequeued; i++) {
		cclient::data::Key *key = kv[i]->getKey ();
		cclient::data::Value *value = kv[i]->getValue ();
		if (NULL != prevMutation) {
			std::pair<char*, size_t> row = key->getRow ();
			if (row.second > 0) {
				std::string rowStr = std::string (row.first, row.second);
				if (prevMutation->getRow () == rowStr) {

					prevMutation->put (key->getColFamilyStr (),
					                   key->getColQualifierStr (),
					                   key->getColVisibilityStr (),
					                   key->getTimeStamp (), key->isDeleted (),
					                   value->data (), value->size ());
					delete kv[i];
					continue;
				}

			}

		}
		cclient::data::Mutation *m = new cclient::data::Mutation (key->getRowStr ());
		m->put (key->getColFamilyStr (), key->getColQualifierStr (),
		        key->getColVisibilityStr (), key->getTimeStamp (),
		        key->isDeleted (), value->data (), value->size ());
		prevMutation = m;
		mutation->push_back (m);
		delete kv[i];

	}
	
	cclient::data::Mutation **mut = new cclient::data::Mutation*[queueSize];
	
	 dequeued = 0;
	for(uint32_t i  = 0; i < queueSize; )
	{
	  if (mutationQueue.try_dequeue(mut[i]))
	  {
	    dequeued++;
	    i++;
	  }
	  else
	    break;
	}
	
	for (uint64_t i = 0; i < dequeued; i++) {
	  mutation->push_back( mut[i] );
	}
	delete [] mut;
	//delete kv;

	binning:
	std::map<std::string, cclient::data::TabletServerMutations*> binnedMutations;
	std::set<std::string> locations;
	try
	{
	  tableLocator->binMutations (credentials, mutation, &binnedMutations,
				      &locations, &failures);
	  for (std::string location : locations) {
		  std::vector<std::string> locationSplit = split (location, ':');
		  cclient::data::tserver::ServerDefinition *rangeDef = new cclient::data::tserver::ServerDefinition (
			  credentials,
			  NULL,
			  locationSplit.at (0), atoi (locationSplit.at (1).c_str ()));
		  writerHeuristic->write (rangeDef, connectorInstance->getConfiguration(), binnedMutations.at (location));

	  }
	}catch(cclient::exceptions::ClientException ce)
	{
	  if (ce.getErrorCode() == NO_LOCATION_IDENTIFIED)
	  {
	    // should check that the table exists
	    bool exists = false;
	    try{
	      exists= tops->exists();
	      
	      
	    }catch(cclient::exceptions::ClientException ce)
	     {
	       throw cclient::exceptions::ClientException(TABLE_NOT_FOUND);
	     }
	     
	     if (!exists)
		   throw cclient::exceptions::ClientException(TABLE_NOT_FOUND);
	   
	  
	      std::this_thread::sleep_for (std::chrono::milliseconds (100));
	      goto binning;
	    
	    
	  }
	  else throw ce;
	
	}catch(apache::thrift::transport::TTransportException tpe)
	{
	  
	  flush(override);

	}
	    delete[] kv;

	    delete mutation;
	}

	      if (override) {
		if ( writerHeuristic->close () != 0)
		  flush(override);
	      }
	
}
	
}

