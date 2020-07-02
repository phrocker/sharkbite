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
#include <iostream>
#include <fstream>
#include <string>
#include <set>
using namespace std;
#include <netinet/in.h>
#include <stdint.h>

#include "../../include/data/constructs/Key.h"
#include "../../include/data/constructs/Range.h"
#include "../../include/data/constructs/KeyExtent.h"
#include "../../include/data/constructs/IterInfo.h"
#include "../../include/data/constructs/value.h"
#include "../../include/data/constructs/KeyValue.h"
#include "../../include/data/constructs/rkey.h"
#include <sys/time.h>
//#include <snappy.h>

#define CATCH_CONFIG_MAIN

#include "../../include/data/constructs/column.h"
#include "../../include/data/constructs/security/Authorizations.h"
#include "../../include/data/constructs/security/AuthInfo.h"

using namespace cclient::data::security;
using namespace cclient::data;
using namespace cclient::data::streams;

#define NUMBER 50000
#include "../catch.hpp"

TEST_CASE("Test IterInfo", "[createAndSet]") {

  IterInfo info("name", "class", 10000);

  REQUIRE(info.getName() == "name");
  REQUIRE(info.getClass() == "class");

  REQUIRE(info.getPriority() == 10000);

}

TEST_CASE("KeyExtents", "[testExtents]") {

  SECTION("BadInputs") {
    KeyExtent *a;
    REQUIRE_THROWS_AS(a = new KeyExtent("", "z", "z"), std::exception);
    REQUIRE_THROWS_AS(a = new KeyExtent("atable", "a", "z"), std::exception);
  }
  SECTION("clippedRanges") {

    KeyExtent a("tableId", "z", "a");

  }
}

TEST_CASE("Key", "Key") {

  SECTION("no data") {
    Key key;
    std::cout << key << std::endl;
  }
  

  SECTION("test null data") {
    Key key;
    key.setRow(0,0);
    key.setColFamily(0,0);
    key.setColQualifier(0,0);
    key.setColVisibility(0,0);
    std::cout << key << std::endl;
  }
}

TEST_CASE("Range", "[testRanges]") {
  SECTION("clippedRanges") {

    auto firstRange = std::make_shared<Range>("a", false, "z", true, false);

    auto range = std::make_shared<Range>("w", true, "z", true, false);
    std::shared_ptr<Range> result = firstRange->intersect(range);

    REQUIRE(result != nullptr);

    REQUIRE(result->getStartKey()->getRowStr() == "w");
    REQUIRE(result->getStopKey()->getRowStr() == "z");
  }
}

