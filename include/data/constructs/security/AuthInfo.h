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

#ifndef AUTHINFO_H_
#define AUTHINFO_H_

#include <string>


namespace cclient
{
namespace data
{
namespace security
{

class AuthInfo
{
public:
    AuthInfo (std::string user, std::string password, std::string instanceId);

    AuthInfo() : user(""), password(""), instanceId("")
    {
      
    }
    virtual
    ~AuthInfo ();

    std::string
    getUserName () const
    {
        return user;
    }

    std::string
    getPassword () const
    {
        return password;
    }

    std::string
    getInstanceId () const
    {
        return instanceId;
    }
    
    AuthInfo &operator=(const AuthInfo &rhs)
    {
      user=rhs.user;
      instanceId=rhs.instanceId;
      password=rhs.password;
      return *this;
    }

protected:
    std::string user;
    std::string instanceId;
    std::string password;
};
}
} /* namespace data */
} /* namespace cclient */
#endif /* AUTHINFO_H_ */
