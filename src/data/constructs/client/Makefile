CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/ -I /usr/local/include/zookeeper -lzookeeper_mt
LDFLAGS=-g ../configuration.so zookeeper/zookeeper_client.so 
LDLIBS=
PWD=`pwd`
RM=rm -f

SRCS=TabletServerMutations.cpp zookeeperinstance.cpp
OBJS=$(subst .cpp,.o,$(SRCS))


all: client

clean: 
	$(RM) $(OBJS)


client:	$(OBJS)
	cd zookeeper ; make clean ; make ; cd $(PWD)
	cd ../configuration ; make clean; make ; cd $(PWD)
	g++ -o client.so $(OBJS) -shared -fPIC
	$(RM) $(OBJS)

TabletServerMutations.o: TabletServerMutations.cpp
	g++ $(CPPFLAGS) -c TabletServerMutations.cpp -fPIC

zookeeperinstance.o: zookeeperinstance.cpp
	g++ $(CPPFLAGS) -c zookeeperinstance.cpp -fPIC
	