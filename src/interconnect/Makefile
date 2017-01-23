CPPFLAGS=-g -std=c++11 -I/usr/local/include/thrift -lthrift -lboost_system -lpthread -lboost_thread -fpermissive -DHAVE_CONFIG_H
LDFLAGS=-g scanrequest/scanrequest.so transport/transport.so
LDLIBS=
RM=rm -f
PWD=`pwd`


#SRCS=InterConnect.cpp TabletServer.cpp
SRCS=InterConnect.cpp ClientInterface.cpp  TransportPool.cpp TabletServer.cpp Scan.cpp Master.cpp
#
OBJS=$(subst .cpp,.o,$(SRCS))


all: interconnect

clean: 
	$(RM) $(OBJS)


interconnect:	$(OBJS)
	cd scanrequest ; make clean ; make ; cd $(PWD)
	cd transport ; make clean;  make ; cd $(PWD)
	g++ -o interconnect.so $(OBJS) -shared
	$(RM) $(OBJS)
	
ClientInterface.o: ClientInterface.cpp 
	g++ $(CPPFLAGS) -c ClientInterface.cpp -fPIC
	
Scan.o: Scan.cpp
	g++ $(CPPFLAGS) -c Scan.cpp -fPIC

#scan.o: Scan.cpp
#	g++ $(CPPFLAGS) -c scan.cpp -o -fPIC


InterConnect.o: InterConnect.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c InterConnect.cpp -fPIC


TabletServer.o: TabletServer.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c TabletServer.cpp -fPIC

Master.o: Master.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c Master.cpp -fPIC
	
		
TransportPool.o: TransportPool.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c TransportPool.cpp -fPIC
	

	