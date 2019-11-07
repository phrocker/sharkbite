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

#ifndef METADATALOCATIONOBTAINER_H_
#define METADATALOCATIONOBTAINER_H_

#include <set>
#include <vector>
#include <algorithm>

#include "TabletLocationObtainer.h"

#include "../constructs/column.h"
#include "../constructs/StructureDefinitions.h"
#include "../constructs/client/Instance.h"
#include "../constructs/security/AuthInfo.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace cclient {
namespace impl {

/**
 * Location obtainer dedicated to retreiving metadata tablets
 **/
class MetaDataLocationObtainer : public TabletLocationObtainer {

 public:
  /**
   * Constructor.
   * @param instance instance
   **/
  explicit MetaDataLocationObtainer(cclient::data::Instance *instance)
      : instance(instance),
        logger(logging::LoggerFactory<MetaDataLocationObtainer>::getLogger()) {
    columns = new std::vector<cclient::data::Column*>();
    columns->push_back(new cclient::data::Column(METADATA_CURRENT_LOCATION_COLUMN_FAMILY));
    columns->push_back(new cclient::data::Column(METADATA_TABLET_COLUMN_FAMILY,
    METADATA_PREV_ROW_COLUMN_CQ));
    std::sort(columns->begin(), columns->end());

  }

  virtual ~MetaDataLocationObtainer();

  /**
   * Finds tablet
   * @param credentials connecting user's credentials
   * @param source source tablet location
   * @param row row to locate
   * @param stopRow end row
   * @param parent parent location obtainer
   * @returns a list of tablet locations for the range
   **/
  std::vector<cclient::data::TabletLocation> findTablet(cclient::data::security::AuthInfo *credentials, cclient::data::TabletLocation *source, std::string row, std::string stopRow,
                                                        TabletLocator *parent);
  /**
   * Finds tablet
   * @param credentials connecting user's credentials
   * @param source source tablet location
   * @param row row to locate
   * @param stopRow end row
   * @param parent parent location obtainer
   * @returns a list of tablet locations for the range
   **/
  std::vector<cclient::data::TabletLocation> findTablet(cclient::data::security::AuthInfo *credentials, std::string tabletserver,
                                                        std::map<cclient::data::KeyExtent, std::vector<cclient::data::Range> > *map, TabletLocator *parent) {
    return std::vector<cclient::data::TabletLocation>();
  }
 protected:
  // columns to locate
  std::vector<cclient::data::Column*> *columns;
  // instance
  cclient::data::Instance *instance;
 private:
  std::shared_ptr<logging::Logger> logger;
};

} /* namespace impl */
} /* namespace cclient */
#endif /* METADATALOCATIONOBTAINER_H_ */

