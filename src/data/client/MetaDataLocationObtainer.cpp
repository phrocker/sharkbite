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
#include <vector>
#include <memory>

#include "data/client/MetaDataLocationObtainer.h"
#include "data/client/../constructs/server/RangeDefinition.h"
#include "data/client/../constructs/StructureDefinitions.h"
#include "data/client/../../interconnect/TabletServer.h"
#include "data/client/../../interconnect/Scan.h"
#include "data/client/../constructs/Range.h"
#include "data/client/../constructs/KeyExtent.h"
#include "data/client/../constructs/Key.h"
#include "data/client/../constructs/IterInfo.h"
#include "data/client/../constructs/value.h"
#include "data/client/TabletLocation.h"

namespace cclient {
namespace impl {

MetaDataLocationObtainer::~MetaDataLocationObtainer() {
}

std::vector<cclient::data::TabletLocation> MetaDataLocationObtainer::findTablet(cclient::data::security::AuthInfo *credentials, cclient::data::TabletLocation *source, std::string row,
                                                                                std::string stopRow, TabletLocator *parent) {
  logging::LOG_TRACE(logger) << "FindTablet " << row << " end:" << stopRow;
  std::vector<cclient::data::TabletLocation> tabletLocations;
  std::shared_ptr<cclient::data::Key> startKey = std::make_shared<cclient::data::Key>();
  startKey->setRow(row.c_str(), row.size());
  std::shared_ptr<cclient::data::Key> endKey = std::make_shared<cclient::data::Key>();
  endKey->setRow(stopRow.c_str(), stopRow.size());
  auto range = std::make_shared<cclient::data::Range>(startKey, true, endKey, true);

  std::map<cclient::data::Key, cclient::data::Value> resultSet;

  cclient::data::security::Authorizations emptyAuths;

  std::vector<std::shared_ptr<cclient::data::Range>> ranges;
  ranges.push_back(range);

  std::vector<std::shared_ptr<cclient::data::KeyExtent>> extents;
  extents.push_back(source->getExtent());
  std::shared_ptr<cclient::data::tserver::RangeDefinition> rangeDef = std::make_shared<cclient::data::tserver::RangeDefinition>(credentials, &emptyAuths, source->getServer(), source->getPort(),
                                                                                                                                &ranges, &extents);

  Configuration conf;
  interconnect::ServerInterconnect directConnect(rangeDef, &conf);

  std::vector<cclient::data::IterInfo> iters;
  iters.emplace_back(cclient::data::IterInfo("WRI", "org.apache.accumulo.core.iterators.user.WholeRowIterator", 10000));
  logging::LOG_TRACE(logger) << "Performing scan  of " << row << " end:" << stopRow << " against " << source->getServer() << ":" << source->getPort() << " on " << source->getExtent();
  std::atomic<bool> isrunning(true);
  interconnect::Scan *initScan = directConnect.scan(&isrunning, columns, iters,100);
  std::vector<std::shared_ptr<cclient::data::KeyValue> > kvResults;
  initScan->getNextResults(&kvResults);

  std::map<std::shared_ptr<cclient::data::Key>, std::shared_ptr<cclient::data::Value>, pointer_comparator<std::shared_ptr<cclient::data::Key>>> results = decodeResults(&kvResults);

  std::shared_ptr<cclient::data::Key> key = 0;
  std::shared_ptr<cclient::data::Value> value = 0;
  std::string lastRowFromKey = "";
  std::string currentRow = "";

  std::string location = "", session = "";

  std::shared_ptr<cclient::data::Value> prevRow = 0;

  for (std::map<std::shared_ptr<cclient::data::Key>, std::shared_ptr<cclient::data::Value> >::iterator it = results.begin(); it != results.end(); it++) {
    key = it->first;
    logging::LOG_DEBUG(logger) << "FindTablet received" << key;
    currentRow = std::string(key->getRow().first, key->getRow().second);
    if (currentRow != lastRowFromKey) {
      prevRow = 0;
      location = session = "";
      lastRowFromKey = currentRow;

    }

    std::pair<char*, size_t> cfBytes = key->getColFamily();
    std::string cf = std::string(cfBytes.first, cfBytes.second);
    std::pair<char*, size_t> cqBytes = key->getColQualifier();
    std::string cq = std::string(cqBytes.first, cqBytes.second);

    value = it->second;
    std::pair<uint8_t*, size_t> valBytes = value->getValue();
    if (cf == METADATA_CURRENT_LOCATION_COLUMN_FAMILY || cf == METADATA_FUTURE_LOCATION_COLUMN_FAMILY) {
      location = std::string((char*) valBytes.first, valBytes.second);
      session = cq;
    } else if (cf == METADATA_TABLET_COLUMN_FAMILY && cq == METADATA_PREV_ROW_COLUMN_CQ) {
      prevRow = value;
    }

    if (prevRow != NULL) {

      std::shared_ptr<cclient::data::KeyExtent> ke = std::make_shared<cclient::data::KeyExtent>(currentRow, prevRow);
      if (location.length() > 0) {
        cclient::data::TabletLocation te(ke, location, session);
        tabletLocations.push_back(te);
      }
    }

  }

  logging::LOG_DEBUG(logger) << "Finished FindTablet received";

  delete initScan;

  return tabletLocations;
}

} /* namespace impl */
} /* namespace cclient */

