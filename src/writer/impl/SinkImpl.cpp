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
#include "writer/impl/SinkImpl.h"

#include "writer/impl/../../data/constructs/Key.h"
#include "writer/impl/../../data/constructs/KeyValue.h"
#include "writer/impl/../../data/constructs/Mutation.h"
#include "writer/impl/../../data/constructs/value.h"
#include "writer/impl/../../data/exceptions/ClientException.h"
#include "writer/impl/WriterHeuristic.h"

namespace writer {
Writer::Writer(std::shared_ptr<cclient::data::Instance> instance,
               interconnect::TableOperations<
                   cclient::data::KeyValue,
                   scanners::ResultBlock<cclient::data::KeyValue>> *tops,
               cclient::data::security::Authorizations *auths, uint16_t threads)
    : tops(tops), Sink<cclient::data::KeyValue>(500), mutationQueue(500 * 1.5) {
  connectorInstance =
      dynamic_pointer_cast<cclient::data::zookeeper::ZookeeperInstance>(
          instance);
  tableLocator = cclient::impl::cachedLocators.getLocator(
      cclient::impl::LocatorKey(connectorInstance, tops->getTableId()));
  credentials = tops->getCredentials();
  writerHeuristic = new WriterHeuristic(threads);
}

Writer::~Writer() {
  if (writerHeuristic->close() > 0) {
    std::vector<std::shared_ptr<cclient::data::Mutation>> failures;
    writerHeuristic->restart_failures(&failures);
    handleFailures(&failures);
    flush(true);
  }
  delete writerHeuristic;
}
void Writer::handleFailures(
    std::vector<std::shared_ptr<cclient::data::Mutation>> *failures) {
  std::vector<std::shared_ptr<cclient::data::Mutation>> newFailures;

  std::map<std::string, std::shared_ptr<cclient::data::TabletServerMutations>>
      binnedMutations;
  std::set<std::string> locations;

  tableLocator->binMutations(credentials, failures, &binnedMutations,
                             &locations, &newFailures);
  for (std::string location : locations) {
    std::vector<std::string> locationSplit = split(location, ':');
    std::shared_ptr<cclient::data::tserver::ServerDefinition> rangeDef =
        std::make_shared<cclient::data::tserver::ServerDefinition>(
            credentials, nullptr, locationSplit.at(0),
            atoi(locationSplit.at(1).c_str()));
    writerHeuristic->write(rangeDef, connectorInstance->getConfiguration(),
                           binnedMutations.at(location));
  }

  writerHeuristic->push_failures(failures);
}
void Writer::flush(bool override) {
  std::vector<std::shared_ptr<cclient::data::Mutation>> failures;
  writerHeuristic->restart_failures(&failures);
  handleFailures(&failures);
  while ((sinkQueue.size_approx() + mutationQueue.size_approx()) > 0) {
    std::vector<std::shared_ptr<cclient::data::KeyValue>> kv;

    size_t dequeued = 0;
    for (int i = 0; i < queueSize; i++) {
      // size_t dequeued = sinkQueue.try_dequeue_bulk (kv, queueSize);
      std::shared_ptr<cclient::data::KeyValue> key;
      if (sinkQueue.try_dequeue(key)) {
        dequeued++;
      }
      kv.push_back(key);
    }

    std::shared_ptr<cclient::data::Mutation> prevMutation = nullptr;
    std::vector<std::shared_ptr<cclient::data::Mutation>> mutation;
    for (size_t i = 0; i < dequeued; i++) {
      std::shared_ptr<cclient::data::Key> key = kv.at(i)->getKey();
      std::shared_ptr<cclient::data::Value> value = kv.at(i)->getValue();
      if (nullptr != prevMutation) {
        std::pair<char *, size_t> row = key->getRow();
        if (row.second > 0) {
          std::string rowStr = std::string(row.first, row.second);
          if (prevMutation->getRow() == rowStr) {
            prevMutation->put(key->getColFamilyStr(), key->getColQualifierStr(),
                              key->getColVisibilityStr(), key->getTimeStamp(),
                              key->isDeleted(), value->data(), value->size());
            continue;
          }
        }
      }
      auto m = std::make_shared<cclient::data::Mutation>(key->getRowStr());
      m->put(key->getColFamilyStr(), key->getColQualifierStr(),
             key->getColVisibilityStr(), key->getTimeStamp(), key->isDeleted(),
             value->data(), value->size());
      prevMutation = m;
      mutation.push_back(m);
    }

    // cclient::data::Mutation **mut = new
    // std::shared_ptr<cclient::data::Mutation>[queueSize];
    std::vector<std::shared_ptr<cclient::data::Mutation>> mut(queueSize);
    // cclient::data::Mutation **mut = new
    // std::shared_ptr<cclient::data::Mutation>[queueSize];

    dequeued = 0;
    for (uint32_t i = 0; i < queueSize;) {
      if (mutationQueue.try_dequeue(mut.at(i))) {
        dequeued++;
        i++;
      } else
        break;
    }

    for (uint64_t i = 0; i < dequeued; i++) {
      mutation.push_back(mut.at(i));
    }

    // delete kv;

  binning:
    std::map<std::string, std::shared_ptr<cclient::data::TabletServerMutations>>
        binnedMutations;
    std::set<std::string> locations;
    try {
      tableLocator->binMutations(credentials, &mutation, &binnedMutations,
                                 &locations, &failures);
      for (std::string location : locations) {
        std::vector<std::string> locationSplit = split(location, ':');
        std::shared_ptr<cclient::data::tserver::ServerDefinition> rangeDef =
            std::make_shared<cclient::data::tserver::ServerDefinition>(
                credentials, nullptr, locationSplit.at(0),
                atoi(locationSplit.at(1).c_str()));
        writerHeuristic->write(rangeDef, connectorInstance->getConfiguration(),
                               binnedMutations.at(location));
      }
    } catch (const cclient::exceptions::ClientException &ce) {
      if (ce.getErrorCode() == NO_LOCATION_IDENTIFIED) {
        // should check that the table exists
        bool exists = false;
        try {
          exists = tops->exists();

        } catch (const cclient::exceptions::ClientException &ce) {
          throw cclient::exceptions::ClientException(TABLE_NOT_FOUND);
        }

        if (!exists)
          throw cclient::exceptions::ClientException(TABLE_NOT_FOUND);

        std::this_thread::sleep_for(std::chrono::milliseconds(100));
        goto binning;

      } else
        throw ce;

    } catch (const apache::thrift::transport::TTransportException &tpe) {
      flush(override);
    }
  }

  if (override) {
    if (writerHeuristic->close() != 0) flush(override);
  }
}

}  // namespace writer
