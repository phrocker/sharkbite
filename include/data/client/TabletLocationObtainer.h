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

#ifndef TABLETLOCATIONOBTAINER_H_
#define TABLETLOCATIONOBTAINER_H_



#include <map>
#include <string>



#include "../constructs/KeyExtent.h"
#include "../constructs/Key.h"
#include "../constructs/KeyValue.h"
#include "../constructs/value.h"
#include "../constructs/Range.h"
#include "../streaming/input/InputStream.h"
#include "ExtentLocator.h"
#include "TabletLocation.h"
namespace cclient {
namespace impl {

/**
 * Tablet location obtainers are meant to obtain tablet locations
 **/
class TabletLocationObtainer {
public:
    TabletLocationObtainer();

    virtual ~TabletLocationObtainer();

    /**
     * Finds tablet 
     * @param credentials connecting user's credentials
     * @param source source tablet location 
     * @param row row to locate
     * @param stopRow end row 
     * @param parent parent location obtainer
     * @returns a list of tablet locations for the range 
     **/
    virtual std::vector<cclient::data::TabletLocation> findTablet(cclient::data::security::AuthInfo *credentials, cclient::data::TabletLocation *source, std::string row,
            std::string stopRow, TabletLocator *parent) = 0;
	    
	/**
     * Finds tablet 
     * @param credentials connecting user's credentials
     * @param source source tablet location 
     * @param row row to locate
     * @param stopRow end row 
     * @param parent parent location obtainer
     * @returns a list of tablet locations for the range 
     **/
    virtual std::vector<cclient::data::TabletLocation> findTablet(cclient::data::security::AuthInfo *credentials, std::string tabletserver,
            std::map<cclient::data::KeyExtent, std::vector<cclient::data::Range> > *map, TabletLocator *parent) = 0;

protected:
    inline std::pair<uint32_t, uint8_t*> readByteArray(cclient::data::streams::InputStream *stream);
    std::map<std::shared_ptr<cclient::data::Key>, std::shared_ptr<cclient::data::Value> ,pointer_comparator<std::shared_ptr<cclient::data::Key> >> decodeResults(std::vector<std::shared_ptr<cclient::data::KeyValue> > *results);
};

} /* namespace data */
} /* namespace cclient */
#endif /* TABLETLOCATIONOBTAINER_H_ */
