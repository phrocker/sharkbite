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
#include "../../../include/data/client/TabletLocation.h"

namespace cclient {
namespace data {

TabletLocation::TabletLocation() :
		tablet_location("") , port(0){
	tablet_extent = std::make_shared<cclient::data::KeyExtent>("!0", "d");
}

TabletLocation::~TabletLocation() {

}

} /* namespace data */
} /* namespace cclient */
