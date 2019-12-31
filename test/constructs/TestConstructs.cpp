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

/*



 TEST_CASE("Test Key", "[createAndSetParameters]") {

 char testVal[3] = { 0x01, 0x02, 0x03 };

 Key key;

 // test reuse
 for (int i = 0; i < 1000; i++) {

 REQUIRE(false == key.isDeleted());

 key.setRow(testVal, 3);
 std::pair<char*, size_t> rowRet = key.getRow();

 REQUIRE(memcmp(testVal, rowRet.first, 3) == 0);
 REQUIRE(3 == rowRet.second);

 key.setColFamily(testVal, 3);
 std::pair<char*, size_t> colFamRet = key.getColFamily();

 REQUIRE(memcmp(testVal, colFamRet.first, 3) == 0);
 REQUIRE(3 == colFamRet.second);

 }

 }

 TEST_CASE("Test KeyExtent", "[createAndSetParameters]") {

 char testVal[3] = { 0x01, 0x02, 0x03 };

 KeyExtent *newExtent = new KeyExtent("tableId","row");
 REQUIRE("tableId" == newExtent->getTableId());

 delete newExtent;


 }

 TEST_CASE("Test Key -- testStreamInterface", "[testStreamInterface]") {

 StreamInterface **kv = new StreamInterface*[25];
 for (int i = 0; i < 25; i++) {
 kv[i] = new KeyValue();
 delete kv[i];
 }

 delete[] kv;

 KeyValue *kvList = new KeyValue[200];

 char tst[400];
 memset(tst, 0x01, 400);

 for (int i = 0; i < 200; i++) {
 REQUIRE(kvList[i].getValue() != NULL);

 kvList[i].getValue()->setValue((uint8_t*) tst, 400);

 REQUIRE(0 ==
 memcmp(tst, kvList[i].getValue()->getValue().first,
 kvList[i].getValue()->getValue().second));

 REQUIRE(kvList[i].getKey() != NULL);
 REQUIRE(kvList[i].getStream() != NULL);

 }

 delete []kvList;

 }




 TEST_CASE("Test Authorizations", "[createAndSetParameters]") {

 vector<string> auths;
 auths.push_back("one");
 auths.push_back("two");

 Authorizations *authorizations = new Authorizations(&auths);

 REQUIRE(2 == authorizations->getAuthorizations().size());

 delete authorizations;
 }

 TEST_CASE("Test Authorizations -- createAuthInfoNoNulls", "[createAuthInfoNoNulls]") {
 AuthInfo *auth = new AuthInfo("testuser", "testpassword", "instance");

 REQUIRE("instance"== auth->getInstanceId());

 REQUIRE("testpassword"== auth->getPassword());

 REQUIRE("testuser"== auth->getUserName());

 delete auth;
 }



 TEST_CASE("Test Column", "[createAndSet]") {

 Column *column = new Column("family", "qualifier", "viz");

 REQUIRE(string(column->getColFamily().first).compare("family") == 0);

 REQUIRE(
 string(column->getColQualifier().first).compare(0,
 column->getColQualifier().second, "qualifier") == 0);

 REQUIRE(
 string(column->getColVisibility().first).compare(0,
 column->getColVisibility().second, "viz") == 0);

 REQUIRE(column->getColFamily().second == strlen("family"));

 REQUIRE(column->getColQualifier().second == strlen("qualifier"));

 REQUIRE(column->getColVisibility().second == strlen("viz"));

 delete column;
 }
 */
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
