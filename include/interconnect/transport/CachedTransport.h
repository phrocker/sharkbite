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

#ifndef CACHEDTRANSPORT_H_
#define CACHEDTRANSPORT_H_

#include <stdexcept>
#include <memory>

#include <string>


#include "Transport.h"
#include "ServerConnection.h"
namespace interconnect
{

template<typename T>
class CachedTransport
{
public:
    CachedTransport(T *transport, ServerConnection *key) :
        ioCount(0), lastCount(-1), reserved(false), threadName(""), foundError(
            false), lastReturnTime(0)
    {
      
	srand (time(NULL));
        cacheKey = new ServerConnection(key);

        serverTransport = transport;
	
	serverTransportPtr = boost::shared_ptr<T>(serverTransport);
    }
    
    ~CachedTransport()
    {
      delete cacheKey;
    }

    void reserve(bool reserve = true)
    {
        reserved = reserve;
    }

    bool isReserved()
    {
        return reserved;
    }

    bool open()
    {
        try
        {
            ioCount++;
            serverTransport->open();
        } catch (std::runtime_error &tfe)
        {
            foundError = true;
            throw tfe;
        }

        ioCount++;

        return (foundError==false);
    }

    void close()
    {
      if (serverTransport != NULL && serverTransport->isOpen())
	serverTransport->close();
    }

    T *getTransport() {
        return serverTransport;
    }

    bool hasError()
    {
        return foundError;
    }

    ServerConnection *getCacheKey()
    {
        return cacheKey;
    }

    boost::shared_ptr<T> getTransporter()
    {
        return serverTransportPtr;
    }

    bool operator ==(const CachedTransport *rhs) const
    {
      return std::addressof(this) == std::addressof(rhs);
        //return *this == *rhs;
    }
/*
    bool operator==(const CachedTransport &rhs) const
    {
        return threadName == rhs.threadName && (*cacheKey == *(rhs.cacheKey));
    }
    */

    bool isOpen()
    {
        return serverTransport->isOpen();
    }

    void registerService(std::string instance, std::string clusterManagers)
    {
        // no op
    }

    void setReturnTime(uint64_t t)
    {
        lastReturnTime = t;
    }

    uint64_t getLastReturnTime()
    {
        return lastReturnTime;
    }
    void sawError(bool sawError)
    {
	foundError = true;
    }

protected:

    bool foundError;
    std::string threadName;
    volatile bool reserved;

    std::string stuckThreadName;

    uint16_t ioCount;
    int16_t lastCount;
    uint64_t lastReturnTime;
    ServerConnection *cacheKey;
    T *serverTransport;
    boost::shared_ptr< T > serverTransportPtr;
};
}
#endif /* CACHEDTRANSPORT_H_ */
