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

#include "../catch.hpp"

#include "../../include/data/constructs/security/Authorizations.h"
#include "../../include/data/constructs/security/AuthInfo.h"

using namespace cclient::data::security;


TEST_CASE("Test Authorizations", "[createAndSetParameters]") {

	list<string> auths;
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


