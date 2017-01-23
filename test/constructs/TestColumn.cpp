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

#include <string>
using namespace std;
#include "../../include/data/constructs/column.h"
#define CATCH_CONFIG_MAIN

#include "../catch.hpp"
using namespace cclient::data;



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
