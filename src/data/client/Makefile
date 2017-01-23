CPPFLAGS=-g2 -std=c++11 -lzookeeper_mt  -I/usr/local/include/thrift/ -lpthread -lthrift -fPIC -DHAVE_CONFIG_H
LDFLAGS=-L/usr/local/lib/  ../../constructs/client/client.so../../constructs/client/zookeeper/zookeeper.so ../../constructs/constructs.so ../../constructs/configuration/Configuration.so ../../streaming/streaming.so -Wl,-rpath=/usr/local/lib/
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: data-client

clean: 
	$(RM) $(OBJS)


data-client:	$(OBJS)
	g++ -o data-client.so $(OBJS) $(LDFLAGS) -shared
	$(RM) $(OBJS)
